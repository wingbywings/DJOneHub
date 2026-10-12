package remotecall

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/iniwex5/vohive/internal/turncred"
)

type restartMedia interface {
	TransportConnected() bool
	CreateRestartOffer(context.Context) (remotemedia.RestartDescription, error)
	AcceptRestartAnswer(remotemedia.RestartDescription) error
}
type restartResult struct {
	offer  remotemedia.RestartDescription
	err    error
	expiry time.Time
	retry  bool
}

// Owned exclusively by RunSignaledCall's select loop. A single offer worker
// returns through a bounded channel. Repeated disconnect notifications and
// late answers cannot extend the ten-second recovery deadline.
type signalRecovery struct {
	enabled                            bool
	media                              restartMedia
	offers                             chan restartResult
	lostAt                             time.Time
	working                            bool
	issued                             bool
	attempts, pending, completed       uint64
	expiry, pendingExpiry, nextAttempt time.Time
	refreshICE                         func(context.Context) (turncred.Credentials, error)
	signalingPaused                    bool
}

func newSignalRecovery(enabled bool, media SignalMedia) *signalRecovery {
	r := &signalRecovery{enabled: enabled, offers: make(chan restartResult, 1)}
	r.media, _ = media.(restartMedia)
	return r
}
func (r *signalRecovery) started() bool { return r.attempts != 0 }
func (r *signalRecovery) tick(ctx context.Context, now time.Time) error {
	if !r.enabled {
		return nil
	}
	if r.media == nil {
		return ErrState
	}
	if !r.pendingExpiry.IsZero() && r.pending == 0 && !r.working && r.media.TransportConnected() {
		r.expiry, r.pendingExpiry = r.pendingExpiry, time.Time{}
	}
	if !r.expiry.IsZero() && !now.Before(r.expiry.Add(-30*time.Second)) {
		return context.DeadlineExceeded
	}
	if !r.lostAt.IsZero() && now.Sub(r.lostAt) >= 10*time.Second {
		return context.DeadlineExceeded
	}
	if r.signalingPaused {
		return nil
	}
	connected := r.media.TransportConnected()
	renew := r.refreshICE != nil && !r.expiry.IsZero() && !now.Before(r.expiry.Add(-2*time.Minute))
	if connected && !r.working && r.pending == 0 {
		r.lostAt = time.Time{}
		r.issued = false
		if !renew || now.Before(r.nextAttempt) {
			return nil
		}
	}
	if r.lostAt.IsZero() {
		r.lostAt = now
	}
	if r.issued || r.working || r.pending != 0 || now.Before(r.nextAttempt) || (!renew && now.Sub(r.lostAt) < 500*time.Millisecond) {
		return nil
	}
	r.working = true
	r.issued = true
	r.attempts++
	deadline := r.lostAt.Add(10 * time.Second)
	previousExpiry := r.expiry
	go func() {
		offerCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		var offer remotemedia.RestartDescription
		var err error
		var expiry time.Time
		if r.refreshICE != nil {
			fetchCtx, stopFetch := context.WithTimeout(offerCtx, 3*time.Second)
			credentials, fetchErr := r.refreshICE(fetchCtx)
			stopFetch()
			if fetchErr != nil {
				select {
				case r.offers <- restartResult{retry: true}:
				case <-ctx.Done():
				}
				return
			}
			renewer, ok := r.media.(interface {
				CreateRestartOfferWithICE(context.Context, turncred.Credentials) (remotemedia.RestartDescription, error)
			})
			if !ok || !credentials.Enabled || credentials.Validate(time.Now()) != nil || credentials.ExpiresAt == nil || credentials.ExpiresAt.Before(previousExpiry) || (renew && !credentials.ExpiresAt.After(previousExpiry)) {
				err = turncred.ErrConfig
			} else {
				expiry = *credentials.ExpiresAt
				offer, err = renewer.CreateRestartOfferWithICE(offerCtx, credentials)
			}
		} else {
			offer, err = r.media.CreateRestartOffer(offerCtx)
		}
		select {
		case r.offers <- restartResult{offer: offer, err: err, expiry: expiry}:
		case <-ctx.Done():
		}
	}()
	return nil
}
func (r *signalRecovery) retry(now time.Time) {
	r.working, r.issued = false, false
	r.nextAttempt = now.Add(5 * time.Second)
	// HTTP failure before touching ICE may retry while the old media works.
	// A genuine transport outage retains its original ten-second budget.
	if r.media.TransportConnected() {
		r.lostAt = time.Time{}
	}
}
func (r *signalRecovery) answer(answer remotemedia.RestartDescription) error {
	if !r.enabled || r.media == nil || answer.ID == 0 {
		return ErrState
	}
	if answer.ID <= r.completed {
		return nil
	} // acknowledged answer replay
	if r.pending == 0 || answer.ID != r.pending {
		return ErrBinding
	}
	if err := r.media.AcceptRestartAnswer(answer); err != nil {
		return err
	}
	r.completed, r.pending, r.working = answer.ID, 0, false
	return nil
}
