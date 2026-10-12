package remotecall

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
)

type controlsBackend struct {
	*testBackend
	digits atomic.Int32
	send   func(context.Context, string) error
}

func (b *controlsBackend) DTMF(ctx context.Context, digit string) error {
	b.digits.Add(1)
	if b.send != nil {
		return b.send(ctx, digit)
	}
	return nil
}

type controlsMedia struct {
	*testMedia
	changes atomic.Int32
	muted   atomic.Bool
}

func (m *controlsMedia) SetMuted(v bool) error { m.changes.Add(1); m.muted.Store(v); return nil }

func controlsSession(t *testing.T, enabled bool) (*Session, *controlsBackend, *controlsMedia) {
	t.Helper()
	cfg := testConfig()
	cfg.CallControls = enabled
	b := &controlsBackend{testBackend: &testBackend{}}
	b.moduleReady.Store(true)
	m := &controlsMedia{testMedia: &testMedia{done: make(chan struct{})}}
	m.connected.Store(true)
	s, err := New(context.Background(), cfg, b, func(context.Context) (Media, error) { return m, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Cancel(); waitDone(t, s) })
	return s, b, m
}
func connectControls(t *testing.T, s *Session) {
	t.Helper()
	submit(t, s, "answer")
	waitFor(t, func() bool { return s.Snapshot().State == "awaiting_audio" })
	if err := s.AudioActive("phone", "call", 7, 1, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Snapshot().State == "connected" })
}
func TestDTMFDeduplicationAndBinding(t *testing.T) {
	s, b, _ := controlsSession(t, true)
	r := request(s, "dtmf")
	r.Digit = "1"
	if _, err := s.Command(context.Background(), "phone", r); err == nil {
		t.Fatal("DTMF before answer")
	}
	connectControls(t, s)
	r = request(s, "dtmf")
	r.Digit = "#"
	for range 3 {
		if _, err := s.Command(context.Background(), "phone", r); err != nil {
			t.Fatal(err)
		}
	}
	if b.digits.Load() != 1 {
		t.Fatal("retry duplicated digit")
	}
	conflict := r
	conflict.Digit = "*"
	if _, err := s.Command(context.Background(), "phone", conflict); !errors.Is(err, callcontrol.ErrRequestConflict) {
		t.Fatal(err)
	}
	if _, err := s.Command(context.Background(), "other", r); !errors.Is(err, ErrBinding) {
		t.Fatal(err)
	}
	conflict = r
	conflict.Generation++
	if _, err := s.Command(context.Background(), "phone", conflict); !errors.Is(err, ErrBinding) {
		t.Fatal(err)
	}
	for _, digit := range []string{"", "12", "A", "1;ATH", "١"} {
		bad := request(s, "dtmf")
		bad.Digit = digit
		if _, err := s.Command(context.Background(), "phone", bad); err == nil {
			t.Fatal("invalid digit", digit)
		}
	}
	if b.digits.Load() != 1 {
		t.Fatal("invalid command reached backend")
	}
}
func TestDTMFInFlightCancellationAndUnknownOutcome(t *testing.T) {
	s, b, _ := controlsSession(t, true)
	connectControls(t, s)
	entered := make(chan struct{})
	b.send = func(ctx context.Context, _ string) error { close(entered); <-ctx.Done(); return ctx.Err() }
	r := request(s, "dtmf")
	r.Digit = "5"
	result := make(chan error, 1)
	go func() { _, err := s.Command(context.Background(), "phone", r); result <- err }()
	<-entered
	other := request(s, "dtmf")
	other.Digit = "6"
	if _, err := s.Command(context.Background(), "phone", other); err == nil {
		t.Fatal("concurrent DTMF")
	}
	// Hanging up is never locked behind slow modem IO.
	submit(t, s, "hangup")
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("hangup blocked by DTMF")
	}
	if _, err := s.Command(context.Background(), "phone", r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if b.digits.Load() != 1 {
		t.Fatal("unknown outcome retried")
	}
	if _, err := s.Command(context.Background(), "phone", other); err == nil {
		t.Fatal("ended call accepted digit")
	}
}
func TestRemoteMuteSequenceAndLegacy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s, _, m := controlsSession(t, enabled)
		if s.SetMuted("phone", "call", 7, 1, true) == nil {
			t.Fatal("pre-answer mute")
		}
		connectControls(t, s)
		err := s.SetMuted("phone", "call", 7, 1, true)
		if !enabled {
			if err == nil {
				t.Fatal("unnegotiated mute")
			}
			r := request(s, "dtmf")
			r.Digit = "1"
			if _, err = s.Command(context.Background(), "phone", r); err == nil {
				t.Fatal("unnegotiated DTMF")
			}
			continue
		}
		if err != nil || !m.muted.Load() {
			t.Fatal("mute failed", err)
		}
		if err = s.SetMuted("phone", "call", 7, 1, true); err != nil || m.changes.Load() != 1 {
			t.Fatal("duplicate mute changed media", err)
		}
		if err = s.SetMuted("phone", "call", 7, 1, false); !errors.Is(err, ErrRevision) {
			t.Fatal("conflicting replay", err)
		}
		if s.SetMuted("other", "call", 7, 2, false) == nil || s.SetMuted("phone", "call", 8, 2, false) == nil {
			t.Fatal("foreign unmute")
		}
		if err = s.SetMuted("phone", "call", 7, 3, false); err != nil || m.muted.Load() {
			t.Fatal(err)
		}
		if err = s.SetMuted("phone", "call", 7, 2, true); !errors.Is(err, ErrRevision) {
			t.Fatal("out of order mute", err)
		}
		submit(t, s, "hangup")
		if s.SetMuted("phone", "call", 7, 4, false) == nil {
			t.Fatal("ended mute")
		}
	}
}
