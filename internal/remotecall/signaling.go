package remotecall

import (
	"context"
	"errors"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/iniwex5/vohive/internal/turncred"
	"github.com/pion/webrtc/v4"
)

// SignalMedia is implemented by the same Peer owned by Session; there is no
// second media path or early audio activation in the signaling adapter.
type SignalMedia interface {
	CreateOffer(context.Context) (remotemedia.Description, error)
	AcceptAnswer(remotemedia.Description) error
	AddCandidate(remotemedia.Candidate) error
	Candidates() <-chan remotemedia.Candidate
}
type SignalCall struct {
	Config
	ModuleID string
	Display  string
	// Requires an upgraded Relay and phone. Default remains the v1 behavior.
	EnableICERestart bool
	// Optional until a durable credential store is supplied by the Hub manager.
	RefreshAccess func(context.Context) (string, error)
	// Reconnect must obtain current credentials and honor ctx for IO/lifetime.
	Reconnect     func(context.Context, callsignal.Message) (*callsignal.Client, error)
	ResumeRefresh func(context.Context) (string, error)
	ICEExpiresAt  time.Time
	RefreshICE    func(context.Context) (turncred.Credentials, error)
}
type SignalFactory func(context.Context) (*Session, SignalMedia, error)

// RunSignaledCall owns and closes the client for one call. Its caller
// serializes calls per Hub. No commands are replayed across client connections;
// negotiated connected calls may resume within a fixed budget; all other
// signaling loss reconciles possible ATA via Finish.
func RunSignaledCall(parent context.Context, client *callsignal.Client, config SignalCall, factory SignalFactory) (Snapshot, error) {
	if config.Route.Mode != callcontrol.Remote || config.Route.TargetInstallationID != config.Binding.InstallationID {
		return Snapshot{}, ErrRoute
	}
	if config.EnableICERestart != config.RecoverTransport {
		return Snapshot{}, ErrState
	}
	if config.RefreshICE != nil && (!config.EnableICERestart || config.ICEExpiresAt.IsZero()) {
		return Snapshot{}, ErrState
	}
	if client == nil || client.Identity().Kind != "hub" || factory == nil {
		return Snapshot{}, ErrBinding
	}
	defer client.Close()
	base := callsignal.Message{Version: 1, HubEpoch: client.Epoch(), ModuleID: config.ModuleID, CallUUID: config.Binding.CallUUID, Generation: config.Binding.Generation, InstallationID: config.Binding.InstallationID, RequestID: callcontrol.NewCallUUID(), Type: "invite", Payload: callsignal.Body(callsignal.Invite{Deadline: config.RingDeadline, Display: config.Display, Direction: func() string {
		if config.Outgoing {
			return "outgoing"
		}
		return ""
	}()})}
	if base.Validate("hub") != nil || !config.RingDeadline.After(time.Now()) || config.RingDeadline.After(time.Now().Add(90*time.Second)) {
		return Snapshot{}, ErrBinding
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Keep the connection until bounded session cleanup and the final state
	// write complete. Closing on caller cancellation used to hide a normal
	// hangup behind peer_closed on the phone.
	if config.RefreshAccess != nil {
		authorizationDone := make(chan struct{})
		go func() { defer close(authorizationDone); _ = client.MaintainAuthorization(ctx, config.RefreshAccess) }()
		defer func() { cancel(); <-authorizationDone }()
	}
	session, media, err := factory(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if session == nil || media == nil {
		if session != nil {
			session.Cancel()
		}
		return Snapshot{}, ErrState
	}
	defer func() { session.Cancel(); <-session.Done(); session.media.Close() }()
	if session.Snapshot().Binding != config.Binding {
		return Snapshot{}, ErrBinding
	}
	type controlMedia interface {
		ControlMessages() <-chan []byte
		ControlReady() bool
		SendControl([]byte) error
	}
	control, _ := media.(controlMedia)
	var controlMessages <-chan []byte
	if config.Reconnect != nil && control != nil {
		controlMessages = control.ControlMessages()
	}
	controlReply := false
	resume := &signalResume{client: client, binding: base, connect: config.Reconnect, refresh: config.ResumeRefresh, results: make(chan resumeResult)}
	defer resume.close()
	send := func(kind, id string, body any) error {
		m := base
		m.Type = kind
		m.RequestID = id
		m.Payload = callsignal.Body(body)
		if (controlReply || resume.pending()) && control != nil && control.ControlReady() && m.ValidateControl("hub") == nil {
			if err := control.SendControl(callsignal.Body(m)); err == nil {
				return nil
			}
		}
		if resume.pending() && kind != "state" {
			return nil
		}
		if resume.working {
			return nil
		}
		if err := resume.client.Send(m); err != nil {
			if session.Snapshot().State == "connected" && resume.enabled() {
				return resume.reconnect(ctx, time.Now())
			}
			return err
		}
		return nil
	}
	if err := client.Send(base); err != nil {
		return session.Snapshot(), err
	}
	// The relay only acknowledges its bounded online queue. Wait for this before
	// sending media; it is not evidence of CallKit success or user acceptance.
	ackTimer := time.NewTimer(5 * time.Second)
	defer ackTimer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	type offered struct {
		description remotemedia.Description
		err         error
	}
	offerResult := make(chan offered, 1)
	recovery := newSignalRecovery(config.EnableICERestart, media)
	recovery.expiry, recovery.refreshICE = config.ICEExpiresAt, config.RefreshICE
	delivered, offerStarted := false, false
	var lastRevision uint64
	var restartOffer *callsignal.RestartSDP
	candidates := media.Candidates()
	sendTerminal := func(snapshot Snapshot) {
		m := base
		m.Type = "state"
		m.RequestID = callcontrol.NewCallUUID()
		m.Payload = callsignal.Body(terminalState(snapshot))
		if control != nil && control.ControlReady() {
			_ = control.SendControl(callsignal.Body(m))
		}
		if !resume.working {
			_ = resume.client.Send(m)
		}
		// Give reliable SCTP a bounded chance to deliver before closing DTLS.
		if config.ControlCleanup && control != nil && control.ControlReady() {
			timer := time.NewTimer(300 * time.Millisecond)
			<-timer.C
		}
	}
	finish := func(reason error) (Snapshot, error) {
		session.Cancel()
		<-session.Done()
		snapshot := session.Snapshot()
		if delivered {
			sendTerminal(snapshot)
		}
		return snapshot, reason
	}
	// Record the failing stage before cancellation can erase it. Only fixed
	// codes cross the status boundary; SDP, candidates and raw errors stay private.
	failStage := func(stage string, err error) (Snapshot, error) {
		session.end(stage)
		return finish(err)
	}
	for {
		var incoming *callsignal.Message
		controlReply = false
		select {
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-resume.done():
			if session.Snapshot().State != "connected" {
				return failStage("signaling_closed", callsignal.ErrConnection)
			}
			if err := resume.reconnect(ctx, time.Now()); err != nil {
				return failStage("signaling_closed", err)
			}
		case result := <-resume.results:
			if err := resume.install(ctx, result); err != nil {
				return failStage("signal_resume_failed", err)
			}
		case <-ackTimer.C:
			if !delivered {
				return failStage("delivery_timeout", callsignal.ErrConnection)
			}
		case <-session.Done():
			snapshot := session.Snapshot()
			if delivered {
				sendTerminal(snapshot)
			}
			return snapshot, nil
		case result := <-offerResult:
			if result.err != nil {
				return failStage("local_offer_failed", callsignal.ErrProtocol)
			}
			if result.description.CallUUID != base.CallUUID || result.description.Generation != base.Generation || result.description.SDP.Type != webrtc.SDPTypeOffer {
				return failStage("local_offer_binding_invalid", ErrBinding)
			}
			if err := send("offer", callcontrol.NewCallUUID(), callsignal.SDP{Type: "offer", SDP: result.description.SDP.SDP, Recovery: config.EnableICERestart, SignalRecovery: resume.enabled(), AudioRecovery: config.RecoverAudio, CallControls: config.CallControls}); err != nil {
				return failStage("signaling_write_failed", err)
			}
		case candidate, ok := <-candidates:
			if !ok {
				candidates = nil
				continue
			}
			if candidate.CallUUID != base.CallUUID || candidate.Generation != base.Generation {
				return failStage("local_ice_binding_invalid", ErrBinding)
			}
			if recovery.started() {
				continue
			} // restart SDP contains its own ICE candidates
			if delivered {
				ice := candidate.ICE
				if err := send("ice", callcontrol.NewCallUUID(), callsignal.ICE{Candidate: ice.Candidate, SDPMid: ice.SDPMid, SDPMLineIndex: ice.SDPMLineIndex, UsernameFragment: ice.UsernameFragment}); err != nil {
					return failStage("signaling_write_failed", err)
				}
			}
		case <-tick.C:
			if !recovery.expiry.IsZero() && !time.Now().Before(recovery.expiry.Add(-30*time.Second)) {
				return failStage("turn_credentials_expired", context.DeadlineExceeded)
			}
			if resume.pending() && !time.Now().Before(resume.deadline) {
				return failStage("signal_recovery_timeout", context.DeadlineExceeded)
			}
			snapshot := session.Snapshot()
			if snapshot.State == "connected" {
				recovery.signalingPaused = resume.pending()
				if err := recovery.tick(ctx, time.Now()); err != nil {
					return failStage("ice_recovery_timeout", err)
				}
			}
			if !resume.pending() && delivered && snapshot.Revision != lastRevision && snapshot.State != "ended" {
				if err := send("state", callcontrol.NewCallUUID(), callsignal.State{Revision: snapshot.Revision, State: snapshot.State}); err != nil {
					return failStage("signaling_write_failed", err)
				}
				lastRevision = snapshot.Revision
			}
		case result := <-recovery.offers:
			if result.retry {
				recovery.retry(time.Now())
				continue
			}
			if result.err != nil {
				return failStage("ice_restart_offer_failed", result.err)
			}
			if result.offer.Description.CallUUID != base.CallUUID || result.offer.Description.Generation != base.Generation {
				return failStage("ice_restart_binding_invalid", ErrBinding)
			}
			recovery.pending = result.offer.ID
			recovery.pendingExpiry = result.expiry
			restartOffer = &callsignal.RestartSDP{ID: result.offer.ID, SDP: result.offer.Description.SDP.SDP}
			if err := send("restart_offer", callcontrol.NewCallUUID(), restartOffer); err != nil {
				return failStage("signaling_write_failed", err)
			}
		case data := <-controlMessages:
			var m callsignal.Message
			if callsignal.Decode(data, &m) != nil || m.ValidateControl("installation") != nil || !base.SameCall(m) ||
				!resume.client.Identity().AccessExpiresAt.After(time.Now()) || (session.Snapshot().State != "connected" && session.Snapshot().State != "ended") {
				return failStage("control_binding_invalid", callsignal.ErrProtocol)
			}
			incoming, controlReply = &m, true
		case m, ok := <-resume.messages():
			if !ok {
				if session.Snapshot().State != "connected" {
					return failStage("signaling_closed", callsignal.ErrConnection)
				}
				if err := resume.reconnect(ctx, time.Now()); err != nil {
					return failStage("signaling_closed", err)
				}
				continue
			}
			incoming = &m
		}
		if incoming == nil {
			continue
		}
		m := *incoming
		if !base.SameCall(m) {
			continue
		} // a late response from an ended call
		switch m.Type {
		case "peer_reconnecting":
			if session.Snapshot().State != "connected" {
				return failStage("signaling_closed", callsignal.ErrConnection)
			}
			if err := resume.pause(time.Now()); err != nil {
				return failStage("signal_recovery_timeout", err)
			}
		case "peer_resumed":
			if !resume.enabled() || !resume.pending() || !time.Now().Before(resume.deadline) {
				return failStage("signal_resume_invalid", ErrState)
			}
			resume.deadline = time.Time{}
			lastRevision = 0 // send authoritative state, never replay ATA/dial
			if restartOffer != nil && recovery.pending != 0 {
				if err := send("restart_offer", callcontrol.NewCallUUID(), restartOffer); err != nil {
					return failStage("signaling_write_failed", err)
				}
			}
		case "waking":
			if !delivered {
				ackTimer.Reset(time.Until(config.RingDeadline))
			}
		case "delivered":
			delivered = true
			ackTimer.Stop()
			if !offerStarted {
				offerStarted = true
				go func() {
					offerCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					defer stop()
					description, err := media.CreateOffer(offerCtx)
					offerResult <- offered{description, err}
				}()
			}
		case "error":
			var failure callsignal.Failure
			callsignal.Decode(m.Payload, &failure)
			if failure.Code == "peer_reconnecting" && session.Snapshot().State == "connected" {
				if err := resume.pause(time.Now()); err != nil {
					return failStage("signal_recovery_timeout", err)
				}
				continue
			}
			session.end("relay_" + relayFailureCode(failure.Code))
			return finish(callsignal.ErrConnection)
		case "peer_closed":
			session.end("peer_closed")
			return finish(callsignal.ErrConnection)
		case "sync":
			snapshot := session.Snapshot()
			state := callsignal.State{Revision: snapshot.Revision, State: snapshot.State, Reason: snapshot.EndReason}
			if snapshot.State == "ended" {
				// End intent is recorded before the bounded hardware cleanup. Never
				// expose it as a completed hangup through a concurrent sync request.
				select {
				case <-session.Done():
					state = terminalState(session.Snapshot())
				default:
					continue
				}
			}
			if err := send("state", m.RequestID, state); err != nil {
				return failStage("signaling_write_failed", err)
			}
		case "command":
			var command callsignal.Command
			callsignal.Decode(m.Payload, &command)
			_, err := session.Command(ctx, m.InstallationID, Request{CallUUID: m.CallUUID, Generation: m.Generation, RequestID: m.RequestID, ExpectedRevision: command.ExpectedRevision, Type: command.Action, Digit: command.Digit})
			code := ""
			if err != nil {
				code = "invalid_state"
				if errors.Is(err, ErrRevision) {
					code = "stale_revision"
				}
			}
			if err := send("result", m.RequestID, callsignal.Result{Accepted: err == nil, Code: code}); err != nil {
				return failStage("signaling_write_failed", err)
			}
		case "mute":
			var mute callsignal.Mute
			callsignal.Decode(m.Payload, &mute)
			err := session.SetMuted(m.InstallationID, m.CallUUID, m.Generation, mute.Sequence, mute.Muted)
			code := ""
			if err != nil {
				code = "invalid_mute_state"
			}
			if err := send("result", m.RequestID, callsignal.Result{Accepted: err == nil, Code: code}); err != nil {
				return failStage("signaling_write_failed", err)
			}
		case "audio_active":
			var audio callsignal.Audio
			callsignal.Decode(m.Payload, &audio)
			err := session.AudioActive(m.InstallationID, m.CallUUID, m.Generation, audio.Sequence, audio.Active)
			code := ""
			if err != nil {
				code = "invalid_audio_state"
			}
			if err := send("result", m.RequestID, callsignal.Result{Accepted: err == nil, Code: code}); err != nil {
				return failStage("signaling_write_failed", err)
			}
		case "answer_sdp":
			if recovery.started() {
				continue
			} // late initial answer cannot mutate a restart
			var sdp callsignal.SDP
			callsignal.Decode(m.Payload, &sdp)
			if media.AcceptAnswer(remotemedia.Description{CallUUID: m.CallUUID, Generation: m.Generation, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp.SDP}}) != nil {
				return failStage("sdp_answer_invalid", callsignal.ErrProtocol)
			}
		case "ice":
			if recovery.started() {
				continue
			}
			var ice callsignal.ICE
			callsignal.Decode(m.Payload, &ice)
			if media.AddCandidate(remotemedia.Candidate{CallUUID: m.CallUUID, Generation: m.Generation, ICE: webrtc.ICECandidateInit{Candidate: ice.Candidate, SDPMid: ice.SDPMid, SDPMLineIndex: ice.SDPMLineIndex, UsernameFragment: ice.UsernameFragment}}) != nil {
				return failStage("remote_ice_invalid", callsignal.ErrProtocol)
			}
		case "restart_answer":
			var answer callsignal.RestartSDP
			callsignal.Decode(m.Payload, &answer)
			if err := recovery.answer(remotemedia.RestartDescription{ID: answer.ID, Description: remotemedia.Description{CallUUID: m.CallUUID, Generation: m.Generation, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}}}); err != nil {
				return failStage("ice_restart_answer_invalid", err)
			}
		}
	}
}

// Only expose codes emitted by our broker. Unknown peer-controlled values may
// be valid protocol IDs but must never become log text or status diagnostics.
func relayFailureCode(code string) string {
	switch code {
	case "unauthorized", "connection_ended", "stale_epoch", "invalid_deadline", "call_unavailable", "connection_capacity", "target_unavailable", "candidate_limit", "push_unavailable", "invite_expired", "push_capacity":
		return code
	default:
		return "unknown_error"
	}
}

// The phone must not claim a completed hangup when cellular teardown remains
// uncertain. Backend error details never cross the signaling boundary.
func terminalState(snapshot Snapshot) callsignal.State {
	reason := snapshot.EndReason
	if snapshot.CleanupError != "" {
		reason = "cleanup_failed"
	}
	return callsignal.State{Revision: snapshot.Revision, State: snapshot.State, Reason: reason}
}
