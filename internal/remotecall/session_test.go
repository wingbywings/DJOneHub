package remotecall

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

type testBackend struct {
	claims, answers, rejects, finishes atomic.Int32
	hangup                             atomic.Bool
	active                             atomic.Bool
	moduleReady                        atomic.Bool
	claimErr, finishErr                error
	answer                             func(context.Context) (bool, error)
}

func (b *testBackend) Claim(ctx context.Context) error {
	b.claims.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.claimErr
}
func (b *testBackend) Answer(ctx context.Context) (bool, error) {
	b.answers.Add(1)
	if b.answer != nil {
		return b.answer(ctx)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	b.active.Store(true)
	return true, nil
}
func (b *testBackend) Reject(context.Context) error { b.rejects.Add(1); return nil }
func (b *testBackend) Observe() (Observation, error) {
	return Observation{CellularActive: b.active.Load(), ModuleReady: b.moduleReady.Load()}, nil
}
func (b *testBackend) Finish(_ context.Context, hangup bool) error {
	b.hangup.Store(hangup)
	b.finishes.Add(1)
	return b.finishErr
}

type testMedia struct {
	connected atomic.Bool
	activated atomic.Int32
	mu        sync.Mutex
	ready     remotemedia.Readiness
	done      chan struct{}
	once      sync.Once
}

func (m *testMedia) TransportConnected() bool { return m.connected.Load() }
func (m *testMedia) UpdateReadiness(state remotemedia.Readiness) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = state
	return nil
}
func (m *testMedia) AnswerReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected.Load() && m.ready.CellularActive && m.ready.ModuleReady
}
func (m *testMedia) Activate() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready.IOSAudioActive || !m.ready.CellularActive || !m.ready.ModuleReady {
		return remotemedia.ErrMediaNotReady
	}
	m.activated.Add(1)
	return nil
}
func (m *testMedia) Done() <-chan struct{} { return m.done }
func (m *testMedia) Close()                { m.once.Do(func() { close(m.done) }) }
func testConfig() Config {
	return Config{Binding: Binding{"call", 7, "phone"}, Route: callcontrol.RouteSnapshot{Mode: callcontrol.Remote, TargetInstallationID: "phone"}, RingDeadline: time.Now().Add(3 * time.Second), AnswerTimeout: time.Second, AudioTimeout: 200 * time.Millisecond, CleanupTimeout: time.Second}
}
func makeSession(t *testing.T, cfg Config, b *testBackend, m *testMedia) *Session {
	t.Helper()
	s, err := New(context.Background(), cfg, b, func(context.Context) (Media, error) { return m, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Cancel(); waitDone(t, s) })
	return s
}
func request(s *Session, kind string) Request {
	snap := s.Snapshot()
	return Request{CallUUID: snap.CallUUID, Generation: snap.Generation, RequestID: callcontrol.NewCallUUID(), ExpectedRevision: snap.Revision, Type: kind}
}
func submit(t *testing.T, s *Session, kind string) Request {
	t.Helper()
	r := request(s, kind)
	if _, err := s.Command(context.Background(), "phone", r); err != nil {
		t.Fatal(err)
	}
	return r
}
func waitDone(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session did not finish")
	}
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAIRouteCreatesNoMedia(t *testing.T) {
	for _, route := range []callcontrol.Route{callcontrol.Agent, callcontrol.Local} {
		cfg := testConfig()
		cfg.Route.Mode = route
		_, err := New(context.Background(), cfg, &testBackend{}, func(context.Context) (Media, error) { t.Fatal("non-remote call created media"); return nil, nil })
		if !errors.Is(err, ErrRoute) {
			t.Fatal(err)
		}
	}
}
func TestSessionAnswerOrderingAndIdempotence(t *testing.T) {
	b, m := &testBackend{}, &testMedia{done: make(chan struct{})}
	b.moduleReady.Store(true)
	s := makeSession(t, testConfig(), b, m)
	r := request(s, "answer")
	if _, err := s.Command(context.Background(), "intruder", r); !errors.Is(err, ErrBinding) {
		t.Fatal(err)
	}
	wrong := r
	wrong.Generation++
	if _, err := s.Command(context.Background(), "phone", wrong); !errors.Is(err, ErrBinding) {
		t.Fatal(err)
	}
	if _, err := s.Command(context.Background(), "phone", r); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.claims.Load() == 1 })
	if b.answers.Load() != 0 {
		t.Fatal("ATA before transport")
	}
	if err := s.AudioActive("phone", "call", 7, 1, true); !errors.Is(err, ErrState) {
		t.Fatal("audio accepted before answer-ready", err)
	}
	m.connected.Store(true)
	waitFor(t, func() bool { return s.Snapshot().State == "awaiting_audio" })
	if m.activated.Load() != 0 {
		t.Fatal("media before audio activation")
	}
	// Retry precedes revision checks, even after the worker advanced state.
	if _, err := s.Command(context.Background(), "phone", r); err != nil {
		t.Fatal(err)
	}
	conflict := r
	conflict.Type = "reject"
	if _, err := s.Command(context.Background(), "phone", conflict); !errors.Is(err, callcontrol.ErrRequestConflict) {
		t.Fatal(err)
	}
	if err := s.AudioActive("phone", "call", 7, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AudioActive("phone", "call", 7, 1, false); !errors.Is(err, ErrRevision) {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Snapshot().State == "connected" })
	if b.answers.Load() != 1 || m.activated.Load() != 1 {
		t.Fatal("duplicate answer/activation")
	}
	hangup := submit(t, s, "hangup")
	waitDone(t, s)
	if !b.hangup.Load() || b.finishes.Load() != 1 {
		t.Fatal("missing call cleanup")
	}
	if _, err := s.Command(context.Background(), "phone", hangup); err != nil {
		t.Fatal("hangup retry", err)
	}
	if err := s.AudioActive("phone", "call", 7, 3, true); err == nil {
		t.Fatal("terminal session resurrected")
	}
}
func TestTimeoutsBeforeAndAfterATA(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		transport, module           bool
		answerTimeout, audioTimeout time.Duration
		wantHangup                  bool
		reason                      string
	}{
		{"transport", false, true, 60 * time.Millisecond, time.Second, false, "transport_unavailable"},
		{"module", true, false, 60 * time.Millisecond, time.Second, true, "answer_timeout"},
		{"audio", true, true, time.Second, 60 * time.Millisecond, true, "audio_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := &testBackend{}, &testMedia{done: make(chan struct{})}
			m.connected.Store(tc.transport)
			b.moduleReady.Store(tc.module)
			cfg := testConfig()
			cfg.AnswerTimeout, cfg.AudioTimeout = tc.answerTimeout, tc.audioTimeout
			s := makeSession(t, cfg, b, m)
			submit(t, s, "answer")
			waitDone(t, s)
			if b.hangup.Load() != tc.wantHangup || b.finishes.Load() != 1 || s.Snapshot().EndReason != tc.reason {
				t.Fatalf("snapshot=%+v hangup=%v", s.Snapshot(), b.hangup.Load())
			}
		})
	}
}
func TestUnknownATAIsReconciledAfterCancellation(t *testing.T) {
	entered := make(chan struct{})
	b, m := &testBackend{}, &testMedia{done: make(chan struct{})}
	m.connected.Store(true)
	b.answer = func(ctx context.Context) (bool, error) { close(entered); <-ctx.Done(); return true, ctx.Err() }
	s := makeSession(t, testConfig(), b, m)
	submit(t, s, "answer")
	<-entered
	// A media failure cancels an in-flight backend operation immediately.
	m.Close()
	waitDone(t, s)
	if !b.hangup.Load() || b.finishes.Load() != 1 {
		t.Fatal("unknown ATA skipped cleanup")
	}
}
func TestRingingExpiryAndLocalWinnerDoNotHangup(t *testing.T) {
	for _, localWinner := range []bool{false, true} {
		b, m := &testBackend{}, &testMedia{done: make(chan struct{})}
		cfg := testConfig()
		cfg.RingDeadline = time.Now().Add(30 * time.Millisecond)
		s := makeSession(t, cfg, b, m)
		if localWinner {
			s.AnsweredElsewhere()
		}
		waitDone(t, s)
		if b.claims.Load() != 0 || b.finishes.Load() != 0 || b.answers.Load() != 0 {
			t.Fatal("unclaimed invitation touched cellular call")
		}
	}
}
func TestRejectDoesNotWaitForICEOrSendATA(t *testing.T) {
	b, m := &testBackend{}, &testMedia{done: make(chan struct{})}
	s := makeSession(t, testConfig(), b, m)
	submit(t, s, "reject")
	waitDone(t, s)
	if b.rejects.Load() != 1 || b.answers.Load() != 0 || b.hangup.Load() || s.Snapshot().EndReason != "rejected" {
		t.Fatal(s.Snapshot())
	}
}
func TestCleanupFailureIsObservable(t *testing.T) {
	b, m := &testBackend{finishErr: errors.New("CLCC unavailable")}, &testMedia{done: make(chan struct{})}
	m.connected.Store(true)
	b.moduleReady.Store(true)
	s := makeSession(t, testConfig(), b, m)
	submit(t, s, "answer")
	waitFor(t, func() bool { return s.Snapshot().State == "awaiting_audio" })
	s.Cancel()
	waitDone(t, s)
	if s.Snapshot().CleanupError != "CLCC unavailable" {
		t.Fatal("cleanup failure hidden")
	}
}
func TestClaimFailureDoesNotReleaseAnotherOwner(t *testing.T) {
	b, m := &testBackend{claimErr: callcontrol.ErrAudioBusy}, &testMedia{done: make(chan struct{})}
	s := makeSession(t, testConfig(), b, m)
	submit(t, s, "answer")
	waitDone(t, s)
	if b.finishes.Load() != 0 || b.answers.Load() != 0 {
		t.Fatal("failed claim touched another lease")
	}
}
