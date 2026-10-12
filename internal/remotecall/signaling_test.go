//go:build cgo

package remotecall

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/pion/webrtc/v4"
)

type signalTestMedia struct {
	*testMedia
	binding                                 Binding
	candidates                              chan remotemedia.Candidate
	offerError, answerError, candidateError error
}

func (m *signalTestMedia) CreateOffer(context.Context) (remotemedia.Description, error) {
	if m.offerError != nil {
		return remotemedia.Description{}, m.offerError
	}
	return remotemedia.Description{CallUUID: m.binding.CallUUID, Generation: m.binding.Generation, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"}}, nil
}
func (m *signalTestMedia) AcceptAnswer(d remotemedia.Description) error {
	if m.answerError != nil {
		return m.answerError
	}
	if d.CallUUID != m.binding.CallUUID || d.Generation != m.binding.Generation {
		return ErrBinding
	}
	m.connected.Store(true)
	return nil
}
func (m *signalTestMedia) AddCandidate(c remotemedia.Candidate) error {
	if m.candidateError != nil {
		return m.candidateError
	}
	if c.CallUUID != m.binding.CallUUID || c.Generation != m.binding.Generation {
		return ErrBinding
	}
	return nil
}
func (m *signalTestMedia) Candidates() <-chan remotemedia.Candidate { return m.candidates }

func signalClients(t *testing.T) (*callsignal.Client, *callsignal.Client, identity.Tokens, *identity.Store) {
	hub, phone, tokens, store, _ := renewableSignalClients(t)
	return hub, phone, tokens, store
}
func renewableSignalClients(t *testing.T, configure ...func(*callsignal.ClientConfig)) (*callsignal.Client, *callsignal.Client, identity.Tokens, *identity.Store, ed25519.PrivateKey) {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var lastKey ed25519.PrivateKey
	proof := func(kind string, g identity.Grant) identity.GrantProof {
		pub, key, _ := ed25519.GenerateKey(nil)
		lastKey = key
		p := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
		p.Signature = ed25519.Sign(key, identity.GrantMessage(kind, p))
		return p
	}
	g, err := store.ProvisionEnrollment(ctx, "tenant", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := store.Enroll(ctx, proof("enrollment", g))
	if err != nil {
		t.Fatal(err)
	}
	g, err = store.CreatePairing(ctx, hub.AccessToken, []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := store.Redeem(ctx, proof("pairing", g))
	if err != nil {
		t.Fatal(err)
	}
	handler := relay.NewServer(store)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(func() { handler.Close(); server.Close(); store.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dial := func(token identity.Tokens) *callsignal.Client {
		config := callsignal.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/v1/signaling", AccessToken: token.AccessToken, IdentityID: token.Principal.ID, Kind: token.Principal.Kind, HubID: token.Principal.HubID, TLSConfig: &tls.Config{RootCAs: roots}}
		for _, option := range configure {
			option(&config)
		}
		c, err := callsignal.Dial(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	return dial(hub), dial(phone), phone, store, lastKey
}
func signalRead(t *testing.T, c *callsignal.Client, kind, state string) callsignal.Message {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-c.Messages():
			if !ok {
				t.Fatal("signaling closed")
			}
			if m.Type == "error" || m.Type == "peer_closed" {
				t.Fatalf("unexpected failure %s", m.Payload)
			}
			if m.Type != kind {
				continue
			}
			if state != "" {
				var s callsignal.State
				callsignal.Decode(m.Payload, &s)
				if s.State != state {
					continue
				}
			}
			return m
		case <-timer.C:
			t.Fatalf("waiting for %s/%s", kind, state)
		}
	}
}
func signalSend(t *testing.T, c *callsignal.Client, m callsignal.Message) {
	t.Helper()
	if err := c.Send(m); err != nil {
		t.Fatal(err)
	}
}

func TestSignaledSessionAnswerAudioGateAndRevocationCleanup(t *testing.T) {
	hub, phone, tokens, store, phoneKey := renewableSignalClients(t)
	config := SignalCall{Config: testConfig(), ModuleID: "module", Display: "test"}
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 7, InstallationID: tokens.Principal.ID}
	config.Route.TargetInstallationID = tokens.Principal.ID
	config.AnswerTimeout = 2 * time.Second
	config.AudioTimeout = time.Second
	config.RingDeadline = time.Now().Add(10 * time.Second)
	b := &testBackend{}
	b.moduleReady.Store(true)
	m := &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
			s, err := New(ctx, config.Config, b, func(context.Context) (Media, error) { return m, nil })
			return s, m, err
		})
		done <- err
	}()
	invite := signalRead(t, phone, "invite", "")
	signalRead(t, phone, "offer", "")
	if b.answers.Load() != 0 {
		t.Fatal("ATA before transport/answer")
	}
	command := invite
	command.Type = "command"
	command.RequestID = callcontrol.NewCallUUID()
	command.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	signalSend(t, phone, command)
	signalRead(t, phone, "result", "")
	signalSend(t, phone, command)
	signalRead(t, phone, "result", "") // same request ID before stale revision check
	if b.answers.Load() != 0 {
		t.Fatal("ATA before transport")
	}
	answer := invite
	answer.Type = "answer_sdp"
	answer.RequestID = callcontrol.NewCallUUID()
	answer.Payload = callsignal.Body(callsignal.SDP{Type: "answer", SDP: "v=0\r\n"})
	signalSend(t, phone, answer)
	signalRead(t, phone, "state", "awaiting_audio")
	if m.activated.Load() != 0 || b.answers.Load() != 1 {
		t.Fatal("audio/answer ordering")
	}
	audio := invite
	audio.Type = "audio_active"
	audio.RequestID = callcontrol.NewCallUUID()
	audio.Payload = callsignal.Body(callsignal.Audio{Sequence: 1, Active: true})
	signalSend(t, phone, audio)
	signalRead(t, phone, "state", "connected")
	if m.activated.Load() != 1 {
		t.Fatal("audio not activated")
	}
	challenge, err := store.Challenge(context.Background(), tokens.Principal.ID, tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	refresh := identity.RefreshProof{IdentityID: tokens.Principal.ID, RefreshToken: tokens.RefreshToken, ChallengeID: challenge.ID, Challenge: challenge.Value}
	refresh.Signature = ed25519.Sign(phoneKey, identity.RefreshMessage(refresh))
	rotated, err := store.Refresh(context.Background(), refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.Renew(context.Background(), rotated.AccessToken); err != nil {
		t.Fatal(err)
	}
	audio.RequestID = callcontrol.NewCallUUID()
	audio.Payload = callsignal.Body(callsignal.Audio{Sequence: 2, Active: true})
	signalSend(t, phone, audio)
	result := signalRead(t, phone, "result", "")
	var accepted callsignal.Result
	callsignal.Decode(result.Payload, &accepted)
	if !accepted.Accepted || m.activated.Load() != 1 || b.answers.Load() != 1 {
		t.Fatal("reauth restarted or invalidated connected call")
	}
	if err := store.AdminRevoke(context.Background(), tokens.Principal.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, callsignal.ErrConnection) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not stop session")
	}
	if b.answers.Load() != 1 || b.finishes.Load() != 1 || !b.hangup.Load() {
		t.Fatal("revocation did not reconcile owned cellular call")
	}
}

func TestSignalingAIRouteDoesNotCreateSession(t *testing.T) {
	config := SignalCall{Config: testConfig(), ModuleID: "module"}
	config.Route.Mode = callcontrol.Agent
	config.RefreshAccess = func(context.Context) (string, error) { t.Fatal("AI started credential refresh"); return "", nil }
	_, err := RunSignaledCall(context.Background(), nil, config, func(context.Context) (*Session, SignalMedia, error) {
		t.Fatal("AI created remote session")
		return nil, nil, nil
	})
	if !errors.Is(err, ErrRoute) {
		t.Fatal(err)
	}
}

func TestSignalingMediaFailurePreservesStageBeforeCancellation(t *testing.T) {
	for _, stage := range []string{"local_offer_failed", "sdp_answer_invalid", "remote_ice_invalid"} {
		t.Run(stage, func(t *testing.T) {
			hub, phone, tokens, _ := signalClients(t)
			config := SignalCall{Config: testConfig(), ModuleID: "module"}
			config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: tokens.Principal.ID}
			config.Route.TargetInstallationID = config.Binding.InstallationID
			b := &testBackend{}
			m := &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}
			privateError := errors.New("untrusted_sdp_or_candidate_secret")
			switch stage {
			case "local_offer_failed":
				m.offerError = privateError
			case "sdp_answer_invalid":
				m.answerError = privateError
			case "remote_ice_invalid":
				m.candidateError = privateError
			}
			type outcome struct {
				snapshot Snapshot
				err      error
			}
			done := make(chan outcome, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				snapshot, err := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
					s, err := New(ctx, config.Config, b, func(context.Context) (Media, error) { return m, nil })
					return s, m, err
				})
				done <- outcome{snapshot, err}
			}()
			invite := signalRead(t, phone, "invite", "")
			if stage != "local_offer_failed" {
				signalRead(t, phone, "offer", "")
				invite.RequestID = callcontrol.NewCallUUID()
				if stage == "sdp_answer_invalid" {
					invite.Type = "answer_sdp"
					invite.Payload = callsignal.Body(callsignal.SDP{Type: "answer", SDP: "v=0\r\n"})
				} else {
					invite.Type = "ice"
					invite.Payload = callsignal.Body(callsignal.ICE{Candidate: "candidate:test"})
				}
				signalSend(t, phone, invite)
			}
			select {
			case result := <-done:
				if result.snapshot.EndReason != stage || !errors.Is(result.err, callsignal.ErrProtocol) {
					t.Fatal("failure stage lost", result.snapshot, result.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("failure did not finish")
			}
			if b.answers.Load() != 0 || b.finishes.Load() != 0 {
				t.Fatal("media failure operated cellular call")
			}
		})
	}
}

func TestSignalingOfflineTargetPreservesRelayFailure(t *testing.T) {
	hub, _, _, _ := signalClients(t)
	config := SignalCall{Config: testConfig(), ModuleID: "module"}
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: "ios_offline"}
	config.Route.TargetInstallationID = config.Binding.InstallationID
	b := &testBackend{}
	m := &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
		s, err := New(ctx, config.Config, b, func(context.Context) (Media, error) { return m, nil })
		return s, m, err
	})
	if !errors.Is(err, callsignal.ErrConnection) || snapshot.EndReason != "relay_target_unavailable" {
		t.Fatal("relay failure lost", snapshot, err)
	}
	if b.answers.Load() != 0 || b.finishes.Load() != 0 {
		t.Fatal("offline target operated cellular call")
	}
	if relayFailureCode("untrusted_secret_value") != "unknown_error" {
		t.Fatal("untrusted failure code escaped into diagnostics")
	}
}
func TestSignalingDisconnectBeforeTransportNeverAnswers(t *testing.T) {
	hub, phone, tokens, _ := signalClients(t)
	config := SignalCall{Config: testConfig(), ModuleID: "module"}
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: tokens.Principal.ID}
	config.Route.TargetInstallationID = tokens.Principal.ID
	b := &testBackend{}
	m := &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
			s, err := New(ctx, config.Config, b, func(context.Context) (Media, error) { return m, nil })
			return s, m, err
		})
		done <- err
	}()
	invite := signalRead(t, phone, "invite", "")
	signalRead(t, phone, "offer", "")
	invite.Type = "command"
	invite.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	signalSend(t, phone, invite)
	signalRead(t, phone, "result", "")
	phone.Close()
	select {
	case err := <-done:
		if !errors.Is(err, callsignal.ErrConnection) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect stuck")
	}
	if b.answers.Load() != 0 || b.hangup.Load() {
		t.Fatal("transport failure answered or hung up unowned call")
	}
}

func TestTerminalStateReportsUnconfirmedCleanupWithoutBackendDetails(t *testing.T) {
	state := terminalState(Snapshot{Revision: 5, State: "ended", EndReason: "remote_hangup", CleanupError: "private backend diagnostic"})
	if state.Reason != "cleanup_failed" || state.State != "ended" || state.Revision != 5 {
		t.Fatal(state)
	}
	state = terminalState(Snapshot{Revision: 4, State: "ended", EndReason: "cellular_ended"})
	if state.Reason != "cellular_ended" {
		t.Fatal("normal caller hangup lost reason")
	}
}
