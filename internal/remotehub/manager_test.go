package remotehub

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
)

type fakeConnection struct {
	done        chan struct{}
	messages    chan callsignal.Message
	once        sync.Once
	maintainers atomic.Int32
}

func fake() *fakeConnection {
	return &fakeConnection{done: make(chan struct{}), messages: make(chan callsignal.Message, 1)}
}
func (f *fakeConnection) Done() <-chan struct{}               { return f.done }
func (f *fakeConnection) Messages() <-chan callsignal.Message { return f.messages }
func (f *fakeConnection) Close()                              { f.once.Do(func() { close(f.done) }) }
func (f *fakeConnection) MaintainAuthorization(ctx context.Context, _ func(context.Context) (string, error)) error {
	f.maintainers.Add(1)
	select {
	case <-ctx.Done():
	case <-f.done:
	}
	return nil
}
func state(t *testing.T, m *Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.Snapshot().State != want {
		if time.Now().After(deadline) {
			t.Fatalf("want %s, got %s", want, m.Snapshot().State)
		}
		time.Sleep(time.Millisecond)
	}
}
func signal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}
func refresh(context.Context) (string, error) { return "", errors.New("unused") }

func TestManagerSerializesCallsAndWaitsForCleanupBeforeReconnect(t *testing.T) {
	first, second := fake(), fake()
	var connects atomic.Int32
	m := Start(context.Background(), Config{Connect: func(context.Context) (Connection, error) {
		if connects.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}, Refresh: refresh})
	cleanup := make(chan struct{})
	defer m.Close()
	// Ensure cleanup is released even if a test assertion fails.
	var release sync.Once
	defer release.Do(func() { close(cleanup) })
	state(t, m, "online")
	running, canceled := make(chan struct{}), make(chan struct{})
	if !m.TryStart(context.Background(), func(ctx context.Context, c Connection) {
		close(running)
		<-ctx.Done()
		close(canceled)
		<-cleanup
	}) {
		t.Fatal("online call rejected")
	}
	signal(t, running)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.TryStart(context.Background(), func(context.Context, Connection) {}) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 0 {
		t.Fatal("more than one call reserved")
	}
	first.Close()
	signal(t, canceled)
	if connects.Load() != 1 {
		t.Fatal("reconnected before cellular cleanup")
	}
	if m.TryStart(context.Background(), func(context.Context, Connection) { t.Error("busy call replayed") }) {
		t.Fatal("cleanup accepted a call")
	}
	release.Do(func() { close(cleanup) })
	state(t, m, "online")
	if connects.Load() != 2 || first.maintainers.Load() != 1 {
		t.Fatal("incorrect connection/authorization ownership")
	}
	complete := make(chan struct{})
	if !m.TryStart(context.Background(), func(_ context.Context, c Connection) {
		if c != second {
			t.Error("old connection reused")
		}
		close(complete)
	}) {
		t.Fatal("new call rejected")
	}
	signal(t, complete)
}

func TestManagerOfflineCanceledAndUnsolicitedCallsNeverRun(t *testing.T) {
	connected := make(chan struct{})
	c := fake()
	m := Start(context.Background(), Config{Connect: func(ctx context.Context) (Connection, error) {
		select {
		case <-connected:
			return c, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, Refresh: refresh})
	defer m.Close()
	var runs atomic.Int32
	run := func(context.Context, Connection) { runs.Add(1) }
	if m.TryStart(context.Background(), run) {
		t.Fatal("queued while offline")
	}
	close(connected)
	state(t, m, "online")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m.TryStart(ctx, run) {
		t.Fatal("queued canceled call")
	}
	c.messages <- callsignal.Message{Type: "command"}
	signal(t, c.done)
	if runs.Load() != 0 {
		t.Fatal("unsolicited/old call ran")
	}
}

func TestManagerShutdownCancelsActiveCallAndJoinsWorkers(t *testing.T) {
	c := fake()
	m := Start(context.Background(), Config{Connect: func(context.Context) (Connection, error) { return c, nil }, Refresh: refresh})
	state(t, m, "online")
	running, finished := make(chan struct{}), make(chan struct{})
	if !m.TryStart(context.Background(), func(ctx context.Context, _ Connection) { close(running); <-ctx.Done(); close(finished) }) {
		t.Fatal("call rejected")
	}
	signal(t, running)
	m.Close()
	signal(t, finished)
	signal(t, c.done)
	if m.Snapshot().State != "stopped" {
		t.Fatal(m.Snapshot())
	}
	m.Close() // idempotent, including concurrent callers
}

func TestAuthorizedIncomingJobReservesConnectionAndJoinsCleanup(t *testing.T) {
	c := fake()
	running := make(chan struct{})
	canceled := make(chan struct{})
	cleanup := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(cleanup) })
	m := Start(context.Background(), Config{Connect: func(context.Context) (Connection, error) { return c, nil }, Refresh: refresh, Incoming: func(ctx context.Context, connection Connection, message callsignal.Message) {
		if connection != c || message.Type != "dial" {
			t.Error("incorrect idle dispatch")
		}
		close(running)
		<-ctx.Done()
		close(canceled)
		<-cleanup
	}})
	defer m.Close()
	defer release.Do(func() { close(cleanup) })
	state(t, m, "online")
	c.messages <- callsignal.Message{Type: "dial"}
	signal(t, running)
	if m.TryStart(context.Background(), func(context.Context, Connection) { t.Error("second job") }) {
		t.Fatal("incoming job did not reserve connection")
	}
	c.Close()
	signal(t, canceled)
	if m.Snapshot().State != "busy" {
		t.Fatal("cleanup not serialized")
	}
	release.Do(func() { close(cleanup) })
}
