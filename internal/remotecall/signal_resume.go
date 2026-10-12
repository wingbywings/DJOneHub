package remotecall

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
)

type resumeResult struct {
	client *callsignal.Client
	cancel context.CancelFunc
	err    error
}

// Owned by the signaling loop. Media/cellular ownership stays in Session.
// A reconnect worker never publishes a client after the owning call ends.
type signalResume struct {
	client   *callsignal.Client
	binding  callsignal.Message
	connect  func(context.Context, callsignal.Message) (*callsignal.Client, error)
	refresh  func(context.Context) (string, error)
	deadline time.Time
	working  bool
	results  chan resumeResult
	cancel   context.CancelFunc
}

func (r *signalResume) enabled() bool { return r.connect != nil && r.client.Identity().Recovery }
func (r *signalResume) pending() bool { return !r.deadline.IsZero() }
func (r *signalResume) messages() <-chan callsignal.Message {
	if r.working {
		return nil
	}
	return r.client.Messages()
}
func (r *signalResume) done() <-chan struct{} {
	if r.working {
		return nil
	}
	return r.client.Done()
}
func (r *signalResume) pause(now time.Time) error {
	if !r.enabled() {
		return callsignal.ErrConnection
	}
	if r.deadline.IsZero() {
		r.deadline = now.Add(10 * time.Second)
	}
	if !now.Before(r.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
func (r *signalResume) reconnect(ctx context.Context, now time.Time) error {
	if err := r.pause(now); err != nil {
		return err
	}
	if r.working {
		return nil
	}
	r.working = true
	if r.cancel != nil {
		r.cancel()
	}
	r.client.Close()
	workerCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	deadline := r.deadline
	go func() {
		// Only the handshake/retry phase has the fixed deadline. A successfully
		// resumed socket inherits the call lifetime, not a soon-expiring deadline.
		timer := time.AfterFunc(time.Until(deadline), cancel)
		defer timer.Stop()
		delay := 250 * time.Millisecond
		var c *callsignal.Client
		var err error
		for workerCtx.Err() == nil {
			c, err = r.connect(workerCtx, r.binding)
			if err == nil && c != nil {
				break
			}
			if c != nil {
				c.Close()
				c = nil
			}
			wait := time.NewTimer(delay)
			select {
			case <-workerCtx.Done():
				wait.Stop()
			case <-wait.C:
			}
			delay = min(time.Second, delay*2)
		}
		timer.Stop()
		if workerCtx.Err() != nil {
			if c != nil {
				c.Close()
				c = nil
			}
			err = workerCtx.Err()
		}
		result := resumeResult{c, cancel, err}
		select {
		case r.results <- result:
		case <-ctx.Done():
			if c != nil {
				c.Close()
			}
			cancel()
		}
	}()
	return nil
}
func (r *signalResume) install(ctx context.Context, result resumeResult) error {
	r.working = false
	if result.err != nil || result.client == nil {
		result.cancel()
		return callsignal.ErrConnection
	}
	old, next := r.client.Identity(), result.client.Identity()
	if !time.Now().Before(r.deadline) || !next.Recovery || !next.Resumed ||
		result.client.Epoch() != r.binding.HubEpoch || old.ID != next.ID || old.Kind != next.Kind || old.HubID != next.HubID {
		result.client.Close()
		result.cancel()
		return callsignal.ErrProtocol
	}
	r.client = result.client
	r.cancel = result.cancel
	if r.refresh != nil {
		c := r.client
		go c.MaintainAuthorization(ctx, r.refresh)
	}
	return nil
}
func (r *signalResume) close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.client.Close()
}
