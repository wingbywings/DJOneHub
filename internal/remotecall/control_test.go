//go:build cgo

package remotecall

import (
	"context"
	"errors"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"sync/atomic"
	"testing"
	"time"
)

type controlTestMedia struct {
	*recoveryMedia
	in, out  chan []byte
	quiesced atomic.Bool
}

func (m *controlTestMedia) ControlMessages() <-chan []byte { return m.in }
func (m *controlTestMedia) ControlReady() bool {
	select {
	case <-m.Done():
		return false
	default:
		return true
	}
}
func (m *controlTestMedia) SendControl(data []byte) error {
	if !m.ControlReady() {
		return errors.New("closed")
	}
	m.out <- append([]byte(nil), data...)
	return nil
}
func (m *controlTestMedia) Quiesce() { m.quiesced.Store(true) }

type controlBackend struct {
	*testBackend
	entered, release chan struct{}
}

func (b *controlBackend) Finish(ctx context.Context, answered bool) error {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.testBackend.Finish(ctx, answered)
}

func TestBackupControlUsesSameHangupAndWaitsForCleanupResult(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "cleanup_failure"}[failCleanup], func(t *testing.T) {
			hub, phone, tokens, _, _ := renewableSignalClients(t, func(c *callsignal.ClientConfig) { c.Resumption = true })
			cfg := SignalCall{Config: testConfig(), ModuleID: "module", Display: "test"}
			cfg.ControlCleanup = true
			cfg.Binding = Binding{callcontrol.NewCallUUID(), 7, tokens.Principal.ID}
			cfg.Route.TargetInstallationID = tokens.Principal.ID
			cfg.Reconnect = func(ctx context.Context, _ callsignal.Message) (*callsignal.Client, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			b := &controlBackend{testBackend: &testBackend{}, entered: make(chan struct{}), release: make(chan struct{})}
			b.moduleReady.Store(true)
			if failCleanup {
				b.finishErr = errors.New("test cleanup failure")
			}
			m := &controlTestMedia{recoveryMedia: &recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: cfg.Binding, candidates: make(chan remotemedia.Candidate)}}, in: make(chan []byte, 8), out: make(chan []byte, 8)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan Snapshot, 1)
			go func() {
				snapshot, _ := RunSignaledCall(ctx, hub, cfg, func(ctx context.Context) (*Session, SignalMedia, error) {
					s, err := New(ctx, cfg.Config, b, func(context.Context) (Media, error) { return m, nil })
					return s, m, err
				})
				done <- snapshot
			}()
			invite := signalRead(t, phone, "invite", "")
			signalRead(t, phone, "offer", "")
			send := func(kind string, body any) {
				msg := invite
				msg.Type, msg.RequestID, msg.Payload = kind, callcontrol.NewCallUUID(), callsignal.Body(body)
				signalSend(t, phone, msg)
			}
			send("answer_sdp", callsignal.SDP{Type: "answer", SDP: "v=0"})
			send("command", callsignal.Command{Action: "answer", ExpectedRevision: 1})
			signalRead(t, phone, "state", "awaiting_audio")
			send("audio_active", callsignal.Audio{Sequence: 1, Active: true})
			connected := signalRead(t, phone, "state", "connected")
			var state callsignal.State
			callsignal.Decode(connected.Payload, &state)
			hub.Close()
			query := invite
			query.Type, query.RequestID, query.Payload = "sync", callcontrol.NewCallUUID(), callsignal.Body(struct{}{})
			m.in <- callsignal.Body(query)
			select {
			case data := <-m.out:
				var got callsignal.Message
				if callsignal.Decode(data, &got) != nil || got.RequestID != query.RequestID || got.Type != "state" {
					t.Fatal("unbound control reply")
				}
			case <-time.After(time.Second):
				t.Fatal("no control response")
			}
			command := invite
			command.Type, command.RequestID, command.Payload = "command", callcontrol.NewCallUUID(), callsignal.Body(callsignal.Command{Action: "hangup", ExpectedRevision: state.Revision})
			m.in <- callsignal.Body(command)
			select {
			case <-b.entered:
			case <-time.After(time.Second):
				t.Fatal("hangup not dispatched")
			}
			if !m.quiesced.Load() || !m.ControlReady() {
				t.Fatal("audio/control lifetime not separated")
			}
			// Status polling and duplicate delivery must not report completion
			// while the hardware cleanup is still blocked.
			m.in <- callsignal.Body(query)
			m.in <- callsignal.Body(command)
			// Drain the intent result, but a terminal success must await cleanup.
			settle := time.NewTimer(100 * time.Millisecond)
		waitCleanup:
			for {
				select {
				case data := <-m.out:
					var got callsignal.Message
					callsignal.Decode(data, &got)
					if got.Type == "state" {
						t.Fatal("premature terminal state")
					}
				case <-settle.C:
					break waitCleanup
				}
			}
			close(b.release)
			var terminal callsignal.State
			deadline := time.After(time.Second)
		receive:
			for {
				select {
				case data := <-m.out:
					var got callsignal.Message
					callsignal.Decode(data, &got)
					if got.Type == "state" {
						callsignal.Decode(got.Payload, &terminal)
						break receive
					}
				case <-deadline:
					t.Fatal("terminal not delivered")
				}
			}
			if terminal.State != "ended" || (failCleanup && terminal.Reason != "cleanup_failed") {
				t.Fatal(terminal)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("control leaked")
			}
			if m.ControlReady() || b.answers.Load() != 1 || b.finishes.Load() != 1 {
				t.Fatal("lifetime or duplicate hardware action")
			}
		})
	}
}
