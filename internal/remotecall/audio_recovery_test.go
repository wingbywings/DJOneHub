//go:build cgo

package remotecall

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type pausedMedia struct {
	*testMedia
	pauses atomic.Int32
}

func (m *pausedMedia) SuspendAudio() error { m.pauses.Add(1); return nil }

func TestAudioInterruptionRecovery(t *testing.T) {
	for _, scenario := range []string{"resume", "immediate_resume", "timeout", "hangup", "cellular_end", "module_lost", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := testConfig()
			cfg.RecoverAudio = scenario != "legacy"
			cfg.AudioRecoveryTimeout = 200 * time.Millisecond
			b := &testBackend{}
			b.moduleReady.Store(true)
			m := &pausedMedia{testMedia: &testMedia{done: make(chan struct{})}}
			m.connected.Store(true)
			s, err := New(context.Background(), cfg, b, func(context.Context) (Media, error) { return m, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Cancel(); waitDone(t, s) }()
			submit(t, s, "answer")
			waitFor(t, func() bool { return s.Snapshot().State == "awaiting_audio" })
			if err = s.AudioActive("phone", "call", 7, 1, true); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return s.Snapshot().State == "connected" })
			if err = s.AudioActive("phone", "call", 7, 2, false); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy" {
				waitDone(t, s)
				if s.Snapshot().EndReason != "audio_deactivated" {
					t.Fatal(s.Snapshot())
				}
				return
			}
			if m.pauses.Load() != 1 || s.Snapshot().State != "connected" {
				t.Fatal("did not suspend synchronously")
			}
			if err = s.AudioActive("phone", "call", 7, 1, true); !errors.Is(err, ErrRevision) {
				t.Fatal("stale activation accepted", err)
			}
			switch scenario {
			case "resume", "immediate_resume":
				if scenario == "resume" {
					waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return !m.ready.IOSAudioActive })
				}
				if err = s.AudioActive("phone", "call", 7, 3, true); err != nil {
					t.Fatal(err)
				}
				waitFor(t, func() bool { return m.activated.Load() == 2 })
				if b.answers.Load() != 1 || s.Snapshot().State != "connected" {
					t.Fatal("resume repeated ATA or changed call")
				}
				// The first interruption's old timer cannot end a resumed call.
				time.Sleep(220 * time.Millisecond)
				if s.Snapshot().State != "connected" {
					t.Fatal("stale timeout ended resumed call")
				}
				submit(t, s, "hangup")
			case "timeout":
				s.mu.Lock()
				deadline := s.audioRecoveryDeadline
				s.mu.Unlock()
				if err = s.AudioActive("phone", "call", 7, 3, false); err != nil {
					t.Fatal(err)
				}
				s.mu.Lock()
				same := s.audioRecoveryDeadline == deadline
				s.mu.Unlock()
				if !same {
					t.Fatal("duplicate deactivation extended deadline")
				}
			case "hangup":
				submit(t, s, "hangup")
			case "cellular_end":
				b.active.Store(false)
			case "module_lost":
				b.moduleReady.Store(false)
			}
			waitDone(t, s)
			if scenario == "timeout" && s.Snapshot().EndReason != "audio_recovery_timeout" {
				t.Fatal(s.Snapshot())
			}
			if b.finishes.Load() != 1 || b.answers.Load() != 1 {
				t.Fatal("incorrect cleanup or duplicate ATA")
			}
			if s.AudioActive("phone", "call", 7, 4, true) == nil {
				t.Fatal("late activation resurrected call")
			}
		})
	}
}
