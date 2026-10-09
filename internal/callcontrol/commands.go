package callcontrol

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrRequestConflict = errors.New("request_id was already used for a different command")
	ErrCommandCapacity = errors.New("command history is full; retry later")
	ErrAudioBusy       = errors.New("another call owns the audio device")
)

// Command includes the actor selected by the trusted caller, never by JSON.
type Command struct {
	RequestID        string `json:"request_id"`
	CallUUID         string `json:"call_uuid"`
	Type             string `json:"type"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Digit            string `json:"digit,omitempty"`
	Actor            string `json:"-"`
}

type Result struct {
	CallUUID string `json:"call_uuid"`
	Type     string `json:"type"`
	Response string `json:"response,omitempty"`
	// Accepted means the modem accepted the command, not that CLCC confirmed it.
	Accepted bool `json:"accepted"`
	// MayHaveAnswered is internal reconciliation state. A timeout after writing
	// ATA still needs call-bound teardown, even without an OK response.
	MayHaveAnswered bool `json:"-"`
}

type commandEntry struct {
	command     Command
	done        chan struct{}
	result      Result
	err         error
	completedAt time.Time
}

// Commands coalesces in-flight retries and retains their results (including
// errors) before callers validate revisions. It does not hold a mutex during IO.
// A zero value is ready to use. Unexpired entries are never evicted to make room.
type Commands struct {
	mu      sync.Mutex
	entries map[string]*commandEntry
}

func (c *Commands) Do(ctx context.Context, command Command, execute func(context.Context) (Result, error)) (Result, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*commandEntry)
	}
	now := time.Now()
	for id, entry := range c.entries {
		if !entry.completedAt.IsZero() && now.Sub(entry.completedAt) > 10*time.Minute {
			delete(c.entries, id)
		}
	}
	if entry := c.entries[command.RequestID]; entry != nil {
		c.mu.Unlock()
		if entry.command != command {
			return Result{}, ErrRequestConflict
		}
		select {
		case <-entry.done:
			return entry.result, entry.err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if len(c.entries) >= 1024 {
		c.mu.Unlock()
		return Result{}, ErrCommandCapacity
	}
	entry := &commandEntry{command: command, done: make(chan struct{})}
	c.entries[command.RequestID] = entry
	c.mu.Unlock()
	result, err := execute(ctx)
	c.mu.Lock()
	entry.result, entry.err, entry.completedAt = result, err, time.Now()
	close(entry.done)
	c.mu.Unlock()
	return result, err
}

type AudioOwner struct{ DeviceID, CallUUID string }
type AudioLease struct {
	Owner      AudioOwner
	Generation uint64
}

// AudioArbiter fences release with a monotonically increasing lease generation.
// Leases are released by confirmed call teardown, not by a wall-clock timer
// that could expire while a modem is still connected.
type AudioArbiter struct {
	mu         sync.Mutex
	lease      AudioLease
	generation uint64
}

func (a *AudioArbiter) Acquire(owner AudioOwner) (AudioLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lease.Generation != 0 {
		if a.lease.Owner == owner {
			return a.lease, nil
		}
		return AudioLease{}, ErrAudioBusy
	}
	a.generation++
	a.lease = AudioLease{Owner: owner, Generation: a.generation}
	return a.lease, nil
}

func (a *AudioArbiter) Release(lease AudioLease) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if lease.Generation == 0 || a.lease != lease {
		return false
	}
	a.lease = AudioLease{}
	return true
}
