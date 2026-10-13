// Package relaystatus exchanges bounded, non-secret runtime observations between
// the Relay and its independent administration process.
package relaystatus

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

const Filename = "runtime-status.json"
const Freshness = 8 * time.Second

var ErrUnavailable = errors.New("runtime observation unavailable")

type Service struct {
	Loaded      bool       `json:"loaded"`
	Environment string     `json:"environment,omitempty"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
	LastResult  string     `json:"last_result,omitempty"`
	Successes   uint64     `json:"successes"`
	Failures    uint64     `json:"failures"`
}

type Activity struct {
	mu sync.Mutex
	s  Service
}

func (a *Activity) Record(success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().UTC()
	a.s.LastAttempt = &now
	a.s.LastResult = "failed"
	if success {
		a.s.LastResult = "accepted"
		a.s.Successes++
	} else {
		a.s.Failures++
	}
}
func (a *Activity) Snapshot(loaded bool, environment string) Service {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	s.Loaded, s.Environment = loaded, environment
	return s
}

type Snapshot struct {
	Version          int       `json:"version"`
	Build            string    `json:"build"`
	ObservedAt       time.Time `json:"observed_at"`
	StartedAt        time.Time `json:"started_at"`
	Running          bool      `json:"running"`
	IdentityHealthy  bool      `json:"identity_healthy"`
	OnlineIdentities []string  `json:"online_identities,omitempty"`
	OnlineCount      int       `json:"online_count"`
	TURN             Service   `json:"turn"`
	APNs             Service   `json:"apns"`
}

type Observation struct {
	State    string    `json:"state"` // fresh, stale, unavailable
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

func Observe(snapshot Snapshot, now time.Time) Observation {
	if snapshot.Version != 1 || snapshot.ObservedAt.IsZero() || snapshot.ObservedAt.After(now.Add(time.Second)) {
		return Observation{State: "unavailable"}
	}
	if now.Sub(snapshot.ObservedAt) > Freshness {
		// Keep the timestamp for diagnosis, but never expose stale online IDs.
		return Observation{State: "stale", Snapshot: &Snapshot{Version: 1, ObservedAt: snapshot.ObservedAt}}
	}
	return Observation{State: "fresh", Snapshot: &snapshot}
}

func (o Observation) Apply(items []identity.ManagedIdentity) {
	for i := range items {
		items[i].SignalState, items[i].SignalObservedAt = "unknown", nil
		if o.State != "fresh" || o.Snapshot == nil {
			continue
		}
		s := o.Snapshot
		items[i].SignalObservedAt = &s.ObservedAt
		items[i].SignalState = "offline"
		if !items[i].Revoked && s.Running {
			for _, id := range s.OnlineIdentities {
				if id == items[i].ID {
					items[i].SignalState = "online"
					break
				}
			}
		}
	}
}

// Start publishes an atomic private snapshot every second. Missing/stale data
// degrades to unknown, never to an inferred online state. stop waits for cleanup.
func Start(parent context.Context, path string, read func(context.Context) Snapshot, reportFailure func()) (func(), error) {
	w, err := newWriter(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	if err = w.write(read(ctx)); err != nil {
		cancel()
		w.close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w.close()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		failed := false
		for {
			select {
			case <-ctx.Done():
				s := read(context.Background())
				s.Running, s.OnlineIdentities, s.OnlineCount = false, nil, 0
				_ = w.write(s)
				return
			case <-tick.C:
				err := w.write(read(ctx))
				if err != nil && !failed && reportFailure != nil {
					reportFailure()
				}
				failed = err != nil
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}
