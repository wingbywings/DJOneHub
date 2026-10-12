//go:build cgo

package remotecall

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

type controlsSignalMedia struct {
	*signalTestMedia
	muted atomic.Bool
}

func (m *controlsSignalMedia) SetMuted(v bool) error { m.muted.Store(v); return nil }

func TestCallControlsThroughAuthenticatedRelay(t *testing.T) {
	hub, phone, tokens, _ := signalClients(t)
	config := SignalCall{Config: testConfig(), ModuleID: "module", Display: "test"}
	config.CallControls = true
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 7, InstallationID: tokens.Principal.ID}
	config.Route.TargetInstallationID = tokens.Principal.ID
	config.RingDeadline = time.Now().Add(10 * time.Second)
	b := &controlsBackend{testBackend: &testBackend{}}
	b.moduleReady.Store(true)
	m := &controlsSignalMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("signaled cleanup timeout")
		}
	}()
	go func() {
		_, err := RunSignaledCall(ctx, hub, config, func(ctx context.Context) (*Session, SignalMedia, error) {
			s, err := New(ctx, config.Config, b, func(context.Context) (Media, error) { return m, nil })
			return s, m, err
		})
		done <- err
	}()
	base := signalRead(t, phone, "invite", "")
	offer := signalRead(t, phone, "offer", "")
	var capabilities callsignal.SDP
	if callsignal.Decode(offer.Payload, &capabilities) != nil || !capabilities.CallControls {
		t.Fatal("controls not advertised")
	}
	send := func(kind string, payload any) callsignal.Message {
		msg := base
		msg.Type = kind
		msg.RequestID = callcontrol.NewCallUUID()
		msg.Payload = callsignal.Body(payload)
		signalSend(t, phone, msg)
		return msg
	}
	ack := func(msg callsignal.Message, want bool) {
		t.Helper()
		result := signalRead(t, phone, "result", "")
		var body callsignal.Result
		callsignal.Decode(result.Payload, &body)
		if result.RequestID != msg.RequestID || body.Accepted != want {
			t.Fatal("incorrect ack", body)
		}
	}
	send("answer_sdp", callsignal.SDP{Type: "answer", SDP: "v=0\r\n"})
	ack(send("command", callsignal.Command{Action: "answer", ExpectedRevision: 1}), true)
	signalRead(t, phone, "state", "awaiting_audio")
	ack(send("audio_active", callsignal.Audio{Sequence: 1, Active: true}), true)
	connected := signalRead(t, phone, "state", "connected")
	var state callsignal.State
	callsignal.Decode(connected.Payload, &state)
	mute := send("mute", callsignal.Mute{Sequence: 1, Muted: true})
	ack(mute, true)
	if !m.muted.Load() {
		t.Fatal("Hub did not mute")
	}
	signalSend(t, phone, mute)
	ack(mute, true)
	ack(send("mute", callsignal.Mute{Sequence: 1, Muted: false}), false)
	if !m.muted.Load() {
		t.Fatal("stale unmute")
	}
	digit := send("command", callsignal.Command{Action: "dtmf", ExpectedRevision: state.Revision, Digit: "#"})
	ack(digit, true)
	signalSend(t, phone, digit)
	ack(digit, true)
	if b.digits.Load() != 1 {
		t.Fatal("WSS replay duplicated hardware tone")
	}
	ack(send("mute", callsignal.Mute{Sequence: 2, Muted: false}), true)
	if m.muted.Load() {
		t.Fatal("unmute lost")
	}
	ack(send("command", callsignal.Command{Action: "hangup", ExpectedRevision: state.Revision}), true)
	signalRead(t, phone, "state", "ended")
}
