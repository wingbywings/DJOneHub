package remotecall

import (
	"context"
	"errors"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
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
	// Optional until a durable credential store is supplied by the Hub manager.
	RefreshAccess func(context.Context) (string, error)
}
type SignalFactory func(context.Context) (*Session, SignalMedia, error)

// RunSignaledCall owns and closes the client for one call. Its caller
// serializes calls per Hub. No commands are replayed across client connections;
// any signaling loss cancels Session and reconciles possible ATA via Finish.
func RunSignaledCall(parent context.Context, client *callsignal.Client, config SignalCall, factory SignalFactory) (Snapshot, error) {
	if config.Route.Mode != callcontrol.Remote || config.Route.TargetInstallationID != config.Binding.InstallationID {
		return Snapshot{}, ErrRoute
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
	defer func() { session.Cancel(); <-session.Done() }()
	if session.Snapshot().Binding != config.Binding {
		return Snapshot{}, ErrBinding
	}
	send := func(kind, id string, body any) error {
		m := base
		m.Type = kind
		m.RequestID = id
		m.Payload = callsignal.Body(body)
		return client.Send(m)
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
	delivered, offerStarted := false, false
	var lastRevision uint64
	candidates := media.Candidates()
	finish := func(reason error) (Snapshot, error) {
		session.Cancel()
		<-session.Done()
		snapshot := session.Snapshot()
		if delivered {
			_ = send("state", callcontrol.NewCallUUID(), terminalState(snapshot))
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
		select {
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-client.Done():
			return failStage("signaling_closed", callsignal.ErrConnection)
		case <-ackTimer.C:
			if !delivered {
				return failStage("delivery_timeout", callsignal.ErrConnection)
			}
		case <-session.Done():
			snapshot := session.Snapshot()
			if delivered {
				_ = send("state", callcontrol.NewCallUUID(), terminalState(snapshot))
			}
			return snapshot, nil
		case result := <-offerResult:
			if result.err != nil {
				return failStage("local_offer_failed", callsignal.ErrProtocol)
			}
			if result.description.CallUUID != base.CallUUID || result.description.Generation != base.Generation || result.description.SDP.Type != webrtc.SDPTypeOffer {
				return failStage("local_offer_binding_invalid", ErrBinding)
			}
			if err := send("offer", callcontrol.NewCallUUID(), callsignal.SDP{Type: "offer", SDP: result.description.SDP.SDP}); err != nil {
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
			if delivered {
				ice := candidate.ICE
				if err := send("ice", callcontrol.NewCallUUID(), callsignal.ICE{Candidate: ice.Candidate, SDPMid: ice.SDPMid, SDPMLineIndex: ice.SDPMLineIndex, UsernameFragment: ice.UsernameFragment}); err != nil {
					return failStage("signaling_write_failed", err)
				}
			}
		case <-tick.C:
			snapshot := session.Snapshot()
			if delivered && snapshot.Revision != lastRevision && snapshot.State != "ended" {
				if err := send("state", callcontrol.NewCallUUID(), callsignal.State{Revision: snapshot.Revision, State: snapshot.State}); err != nil {
					return failStage("signaling_write_failed", err)
				}
				lastRevision = snapshot.Revision
			}
		case m, ok := <-client.Messages():
			if !ok {
				return failStage("signaling_closed", callsignal.ErrConnection)
			}
			if !base.SameCall(m) {
				continue
			} // a late response from an ended call
			switch m.Type {
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
				session.end("relay_" + relayFailureCode(failure.Code))
				return finish(callsignal.ErrConnection)
			case "peer_closed":
				session.end("peer_closed")
				return finish(callsignal.ErrConnection)
			case "command":
				var command callsignal.Command
				callsignal.Decode(m.Payload, &command)
				_, err := session.Command(ctx, m.InstallationID, Request{CallUUID: m.CallUUID, Generation: m.Generation, RequestID: m.RequestID, ExpectedRevision: command.ExpectedRevision, Type: command.Action})
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
				var sdp callsignal.SDP
				callsignal.Decode(m.Payload, &sdp)
				if media.AcceptAnswer(remotemedia.Description{CallUUID: m.CallUUID, Generation: m.Generation, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp.SDP}}) != nil {
					return failStage("sdp_answer_invalid", callsignal.ErrProtocol)
				}
			case "ice":
				var ice callsignal.ICE
				callsignal.Decode(m.Payload, &ice)
				if media.AddCandidate(remotemedia.Candidate{CallUUID: m.CallUUID, Generation: m.Generation, ICE: webrtc.ICECandidateInit{Candidate: ice.Candidate, SDPMid: ice.SDPMid, SDPMLineIndex: ice.SDPMLineIndex, UsernameFragment: ice.UsernameFragment}}) != nil {
					return failStage("remote_ice_invalid", callsignal.ErrProtocol)
				}
			}
		}
	}
}

// Only expose codes emitted by our broker. Unknown peer-controlled values may
// be valid protocol IDs but must never become log text or status diagnostics.
func relayFailureCode(code string) string {
	switch code {
	case "unauthorized", "connection_ended", "stale_epoch", "invalid_deadline", "call_unavailable", "connection_capacity", "target_unavailable", "candidate_limit":
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
