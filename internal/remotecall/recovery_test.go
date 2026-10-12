//go:build cgo

package remotecall

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/pion/webrtc/v4"
)

type recoveryMedia struct {
	*signalTestMedia
	offersMade, answersAccepted atomic.Uint64
}

func (m *recoveryMedia) CreateRestartOffer(ctx context.Context) (remotemedia.RestartDescription, error) {
	if err := ctx.Err(); err != nil {
		return remotemedia.RestartDescription{}, err
	}
	return remotemedia.RestartDescription{ID: m.offersMade.Add(1), Description: remotemedia.Description{CallUUID: m.binding.CallUUID, Generation: m.binding.Generation, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"}}}, nil
}
func (m *recoveryMedia) AcceptRestartAnswer(answer remotemedia.RestartDescription) error {
	m.answersAccepted.Add(1)
	m.connected.Store(true)
	return nil
}
func TestRecoveryHasOneAttemptAndFixedDeadline(t *testing.T) {
	m := &recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{}}}
	r := newSignalRecovery(true, m)
	now := time.Now()
	if err := r.tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(context.Background(), now.Add(499*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if m.offersMade.Load() != 0 {
		t.Fatal("no transient grace")
	}
	if err := r.tick(context.Background(), now.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-r.offers:
		if result.err != nil {
			t.Fatal(result.err)
		}
		r.pending = result.offer.ID
	case <-time.After(time.Second):
		t.Fatal("offer worker blocked")
	}
	for second := 1; second < 10; second++ {
		if err := r.tick(context.Background(), now.Add(time.Duration(second)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if m.offersMade.Load() != 1 {
		t.Fatal("restart storm")
	}
	if err := r.tick(context.Background(), now.Add(10*time.Second)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline extended", err)
	}
}
func TestRecoveryReplyCannotCompleteAnotherAttempt(t *testing.T) {
	m := &recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{}}}
	r := newSignalRecovery(true, m)
	r.pending, r.working, r.attempts, r.issued = 2, true, 2, true
	r.completed = 1
	if err := r.answer(remotemedia.RestartDescription{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if !r.working || r.pending != 2 || m.answersAccepted.Load() != 0 {
		t.Fatal("old answer changed state")
	}
	if err := r.answer(remotemedia.RestartDescription{ID: 3}); !errors.Is(err, ErrBinding) {
		t.Fatal(err)
	}
	if err := r.answer(remotemedia.RestartDescription{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.answer(remotemedia.RestartDescription{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if m.answersAccepted.Load() != 1 {
		t.Fatal("duplicate answer applied")
	}
	if err := r.tick(context.Background(), time.Now()); err != nil || r.issued {
		t.Fatal("recovery did not reset")
	}
}
func TestRestartTraversesRelayWithoutAnsweringCellularAgain(t *testing.T) {
	hub, phone, tokens, _ := signalClients(t)
	config := SignalCall{Config: testConfig(), ModuleID: "module", Display: "test", EnableICERestart: true}
	config.RecoverTransport = true
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 7, InstallationID: tokens.Principal.ID}
	config.Route.TargetInstallationID = tokens.Principal.ID
	config.RingDeadline = time.Now().Add(10 * time.Second)
	b := &testBackend{}
	b.moduleReady.Store(true)
	m := &recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan Snapshot, 1)
	go func() {
		snapshot, _ := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
			s, err := New(ctx, config.Config, b, func(context.Context) (Media, error) { return m, nil })
			return s, m, err
		})
		done <- snapshot
	}()
	invite := signalRead(t, phone, "invite", "")
	offer := signalRead(t, phone, "offer", "")
	var initial callsignal.SDP
	if callsignal.Decode(offer.Payload, &initial) != nil || !initial.Recovery {
		t.Fatal("recovery not advertised")
	}
	send := func(kind string, body any) {
		message := invite
		message.Type, message.RequestID, message.Payload = kind, callcontrol.NewCallUUID(), callsignal.Body(body)
		signalSend(t, phone, message)
	}
	send("answer_sdp", callsignal.SDP{Type: "answer", SDP: "v=0\r\n"})
	send("command", callsignal.Command{Action: "answer", ExpectedRevision: 1})
	signalRead(t, phone, "state", "awaiting_audio")
	send("audio_active", callsignal.Audio{Sequence: 1, Active: true})
	signalRead(t, phone, "state", "connected")
	m.connected.Store(false)
	restart := signalRead(t, phone, "restart_offer", "")
	var proposal callsignal.RestartSDP
	if callsignal.Decode(restart.Payload, &proposal) != nil || proposal.ID != 1 || !restart.SameCall(invite) {
		t.Fatal("restart changed call binding")
	}
	send("restart_answer", callsignal.RestartSDP{ID: proposal.ID, SDP: "v=0\r\n"})
	deadline := time.Now().Add(time.Second)
	for m.answersAccepted.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("restart answer not applied")
		}
		time.Sleep(time.Millisecond)
	}
	if b.answers.Load() != 1 || m.activated.Load() != 1 {
		t.Fatal("restart repeated ATA or activation")
	}
	cancel()
	select {
	case snapshot := <-done:
		if snapshot.State != "ended" || snapshot.CleanupError != "" {
			t.Fatal(snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("restart leaked call cleanup")
	}
}
