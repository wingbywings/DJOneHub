package callcontrol

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommandsCoalesceAndRejectChangedRetry(t *testing.T) {
	var ledger Commands
	command := Command{RequestID: "r", CallUUID: "c", Type: "answer", Actor: "local", ExpectedRevision: 1}
	entered, release := make(chan struct{}), make(chan struct{})
	var executions atomic.Int32
	execute := func(context.Context) (Result, error) {
		executions.Add(1)
		close(entered)
		<-release
		return Result{CallUUID: "c", Accepted: true}, nil
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := ledger.Do(context.Background(), command, execute)
			if err != nil || !result.Accepted {
				t.Errorf("result=%+v err=%v", result, err)
			}
		}()
	}
	<-entered
	changed := command
	changed.CallUUID = "next"
	if _, err := ledger.Do(context.Background(), changed, execute); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ledger.Do(ctx, command, execute); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation=%v", err)
	}
	close(release)
	wg.Wait()
	if executions.Load() != 1 {
		t.Fatalf("executions=%d", executions.Load())
	}
	if _, err := ledger.Do(context.Background(), command, execute); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatal("completed retry executed again")
	}
}

func TestCommandsRetainErrorsAndBoundHistory(t *testing.T) {
	var ledger Commands
	failure := errors.New("ambiguous modem timeout")
	command := Command{RequestID: "r", CallUUID: "c", Type: "dtmf"}
	_, _ = ledger.Do(context.Background(), command, func(context.Context) (Result, error) { return Result{}, failure })
	if _, err := ledger.Do(context.Background(), command, func(context.Context) (Result, error) { t.Fatal("error retry executed"); return Result{}, nil }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	for i := 0; i < 1024; i++ {
		ledger.entries[time.Unix(int64(i), 0).String()] = &commandEntry{completedAt: time.Now()}
	}
	ledger.mu.Unlock()
	command.RequestID = "new"
	if _, err := ledger.Do(context.Background(), command, nil); !errors.Is(err, ErrCommandCapacity) {
		t.Fatalf("capacity=%v", err)
	}
	ledger.mu.Lock()
	for _, entry := range ledger.entries {
		entry.completedAt = time.Now().Add(-11 * time.Minute)
	}
	ledger.mu.Unlock()
	if _, err := ledger.Do(context.Background(), command, func(context.Context) (Result, error) { return Result{}, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestAudioArbiterFencesOldRelease(t *testing.T) {
	var arbiter AudioArbiter
	owner := AudioOwner{DeviceID: "module", CallUUID: "call"}
	first, err := arbiter.Acquire(owner)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := arbiter.Acquire(owner)
	if same != first {
		t.Fatal("same owner changed lease")
	}
	if _, err := arbiter.Acquire(AudioOwner{DeviceID: "other", CallUUID: "second"}); !errors.Is(err, ErrAudioBusy) {
		t.Fatal(err)
	}
	if !arbiter.Release(first) {
		t.Fatal("release failed")
	}
	second, _ := arbiter.Acquire(owner)
	if arbiter.Release(first) {
		t.Fatal("old generation released new lease")
	}
	if !arbiter.Release(second) {
		t.Fatal("new lease lost")
	}
}
