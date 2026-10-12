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

func TestCallResumesWSSWithoutRepeatingCellularAnswer(t *testing.T) {
	for _, side := range []string{"hub", "phone", "both", "retry", "cancel", "timeout", "survives_deadline"} {
		t.Run(side, func(t *testing.T) {
			var hubConfig, phoneConfig callsignal.ClientConfig
			hub, phone, tokens, _, _ := renewableSignalClients(t, func(c *callsignal.ClientConfig) {
				c.Resumption = true
				if c.Kind == "hub" {
					hubConfig = *c
				} else {
					phoneConfig = *c
				}
			})
			config := SignalCall{Config: testConfig(), ModuleID: "module", Display: "test", EnableICERestart: true}
			config.RecoverTransport = true
			config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 7, InstallationID: tokens.Principal.ID}
			config.Route.TargetInstallationID = tokens.Principal.ID
			config.RingDeadline = time.Now().Add(20 * time.Second)
			var attempts atomic.Int32
			config.Reconnect = func(ctx context.Context, binding callsignal.Message) (*callsignal.Client, error) {
				attempt := attempts.Add(1)
				if side == "cancel" || side == "timeout" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if side == "retry" && attempt == 1 {
					return nil, errors.New("temporary network loss")
				}
				cfg := hubConfig
				cfg.Resume = &binding
				return callsignal.Dial(ctx, cfg)
			}
			b := &testBackend{}
			b.moduleReady.Store(true)
			m := &recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
			signalRead(t, phone, "offer", "")
			send := func(kind string, body any) callsignal.Message {
				message := invite
				message.Type, message.RequestID, message.Payload = kind, callcontrol.NewCallUUID(), callsignal.Body(body)
				signalSend(t, phone, message)
				return message
			}
			send("answer_sdp", callsignal.SDP{Type: "answer", SDP: "v=0\r\n"})
			send("command", callsignal.Command{Action: "answer", ExpectedRevision: 1})
			signalRead(t, phone, "state", "awaiting_audio")
			send("audio_active", callsignal.Audio{Sequence: 1, Active: true})
			state := signalRead(t, phone, "state", "connected")
			if side != "phone" {
				hub.Close()
			}
			if side == "cancel" {
				deadline := time.Now().Add(time.Second)
				for attempts.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				cancel()
			} else if side != "timeout" {
				if side == "phone" || side == "both" {
					phone.Close()
					phoneConfig.Resume = &invite
					var err error
					phone, err = callsignal.Dial(ctx, phoneConfig)
					if err != nil {
						t.Fatal(err)
					}
					defer phone.Close()
				}
				signalRead(t, phone, "peer_resumed", "")
				sync := send("sync", struct{}{})
				// The authoritative reply must be correlated, even with unchanged revision.
				for {
					state = signalRead(t, phone, "state", "connected")
					if state.RequestID == sync.RequestID {
						break
					}
				}
				if !state.SameCall(invite) {
					t.Fatal("recovery changed binding")
				}
				if b.answers.Load() != 1 || m.activated.Load() != 1 {
					t.Fatal("repeated hardware action")
				}
				if side == "survives_deadline" {
					// A resumed socket belongs to the call, not the expired retry budget.
					select {
					case snapshot := <-done:
						t.Fatal("resumed socket kept recovery deadline", snapshot)
					case <-time.After(10500 * time.Millisecond):
					}
					query := send("sync", struct{}{})
					for {
						state = signalRead(t, phone, "state", "connected")
						if state.RequestID == query.RequestID {
							break
						}
					}
				}
				var current callsignal.State
				callsignal.Decode(state.Payload, &current)
				send("command", callsignal.Command{Action: "hangup", ExpectedRevision: current.Revision})
			}
			select {
			case snapshot := <-done:
				if side == "timeout" && snapshot.EndReason != "signal_recovery_timeout" && snapshot.EndReason != "signal_resume_failed" {
					t.Fatal("unexpected timeout reason", snapshot.EndReason)
				}
				if snapshot.State != "ended" || snapshot.CleanupError != "" {
					t.Fatal(snapshot)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("call did not clean up")
			}
			if side == "retry" && attempts.Load() != 2 {
				t.Fatal("bounded retry missing", attempts.Load())
			}
		})
	}
}
