//go:build cgo && remoteopus

package remotecall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/pion/webrtc/v4"
)

func TestWSSNegotiatesActualOpusPeerBeforeATA(t *testing.T) {
	hub, phone, tokens, _ := signalClients(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	config := SignalCall{Config: testConfig(), ModuleID: "module"}
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: tokens.Principal.ID}
	config.Route.TargetInstallationID = tokens.Principal.ID
	config.RingDeadline = time.Now().Add(12 * time.Second)
	config.AnswerTimeout = 8 * time.Second
	peerConfig := remotemedia.PeerConfig{CallUUID: config.Binding.CallUUID, Generation: config.Binding.Generation}
	remote, err := remotemedia.NewPeer(ctx, peerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	b := &testBackend{}
	b.moduleReady.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
			var peer *remotemedia.Peer
			session, err := New(ctx, config.Config, b, func(ctx context.Context) (Media, error) {
				var err error
				peer, err = remotemedia.NewPeer(ctx, peerConfig)
				return peer, err
			})
			return session, peer, err
		})
		done <- err
	}()
	invite := signalRead(t, phone, "invite", "")
	offer := signalRead(t, phone, "offer", "")
	var sdp callsignal.SDP
	if callsignal.Decode(offer.Payload, &sdp) != nil {
		t.Fatal("invalid offer")
	}
	if b.answers.Load() != 0 {
		t.Fatal("ATA before answer")
	}
	command := invite
	command.Type = "command"
	command.RequestID = callcontrol.NewCallUUID()
	command.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	signalSend(t, phone, command)
	signalRead(t, phone, "result", "")
	if b.answers.Load() != 0 {
		t.Fatal("ATA before ICE/DTLS")
	}
	answer, err := remote.AcceptOffer(ctx, remotemedia.Description{CallUUID: config.Binding.CallUUID, Generation: config.Binding.Generation, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp.SDP}})
	if err != nil {
		t.Fatal(err)
	}
	m := invite
	m.Type = "answer_sdp"
	m.RequestID = callcontrol.NewCallUUID()
	m.Payload = callsignal.Body(callsignal.SDP{Type: "answer", SDP: answer.SDP.SDP})
	signalSend(t, phone, m)
	// This uses real ICE over host interfaces, unlike the 3-second mock-state
	// helper. Keep consuming signaling and use the configured answer budget;
	// an early transport/session failure must be reported, not hidden by polling.
	wait := time.NewTimer(config.AnswerTimeout)
	defer wait.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for b.answers.Load() != 1 || !remote.TransportConnected() {
		select {
		case err := <-done:
			t.Fatalf("session ended before ATA/transport readiness: %v; remote: %v", err, remote.Err())
		case <-phone.Done():
			t.Fatal("phone signaling closed before transport readiness")
		case message := <-phone.Messages():
			if message.Type == "error" || message.Type == "peer_closed" {
				t.Fatalf("signaling failed: %s", message.Payload)
			}
		case <-wait.C:
			t.Fatalf("answer budget exceeded: answers=%d connected=%v remote=%v", b.answers.Load(), remote.TransportConnected(), remote.Err())
		case <-tick.C:
		}
	}
	if !remote.TransportConnected() {
		t.Fatal("actual peer transport did not connect")
	}
	phone.Close()
	select {
	case err := <-done:
		if !errors.Is(err, callsignal.ErrConnection) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup stalled")
	}
	if !b.hangup.Load() || b.finishes.Load() != 1 {
		t.Fatal("cellular cleanup not reconciled")
	}
}
