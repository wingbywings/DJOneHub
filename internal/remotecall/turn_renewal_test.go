//go:build cgo

package remotecall

import (
	"context"
	"errors"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/iniwex5/vohive/internal/turncred"
	"strings"
	"testing"
	"time"
)

type renewingMedia struct{ *recoveryMedia }

func (m *renewingMedia) CreateRestartOfferWithICE(ctx context.Context, c turncred.Credentials) (remotemedia.RestartDescription, error) {
	m.connected.Store(false)
	return m.CreateRestartOffer(ctx)
}
func TestTURNRenewalConfirmsTransportBeforeExtendingExpiry(t *testing.T) {
	now := time.Now()
	m := &renewingMedia{&recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{}}}}
	m.connected.Store(true)
	issuer, _ := turncred.New(turncred.Config{URLs: []string{"turn:127.0.0.1:3478?transport=udp"}, Secret: strings.Repeat("k", 40), RelayOnly: true})
	fresh, _ := issuer.Issue(now)
	r := newSignalRecovery(true, m)
	old := now.Add(2 * time.Minute)
	r.expiry = old
	calls := 0
	r.refreshICE = func(context.Context) (turncred.Credentials, error) { calls++; return fresh, nil }
	if err := r.tick(context.Background(), now.Add(-time.Second)); err != nil || r.working {
		t.Fatal("early renewal", err)
	}
	if err := r.tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	result := <-r.offers
	if result.err != nil || result.retry {
		t.Fatal(result.err)
	}
	r.pending, r.pendingExpiry = result.offer.ID, result.expiry
	if err := r.tick(context.Background(), now.Add(time.Second)); err != nil || r.expiry != old {
		t.Fatal("extended before answer", err)
	}
	if err := r.answer(result.offer); err != nil {
		t.Fatal(err)
	}
	m.connected.Store(false)
	if err := r.tick(context.Background(), now.Add(2*time.Second)); err != nil || r.expiry != old {
		t.Fatal("extended before allocation connected", err)
	}
	m.connected.Store(true)
	if err := r.tick(context.Background(), now.Add(3*time.Second)); err != nil || !r.expiry.Equal(*fresh.ExpiresAt) {
		t.Fatal("renewal not committed", err)
	}
	if calls != 1 || m.offersMade.Load() != 1 {
		t.Fatal("duplicate negotiation")
	}
}
func TestTURNRenewalFetchRetryKeepsOriginalExpiryAndStopsAtDeadline(t *testing.T) {
	now := time.Now()
	m := &renewingMedia{&recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{}}}}
	m.connected.Store(true)
	r := newSignalRecovery(true, m)
	r.expiry = now.Add(2 * time.Minute)
	r.refreshICE = func(context.Context) (turncred.Credentials, error) {
		return turncred.Credentials{}, errors.New("issuer unreachable")
	}
	if err := r.tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	result := <-r.offers
	if !result.retry || m.offersMade.Load() != 0 {
		t.Fatal("modified ICE on HTTP failure")
	}
	r.retry(now)
	if err := r.tick(context.Background(), now.Add(time.Second)); err != nil || r.working {
		t.Fatal("unbounded retry")
	}
	if err := r.tick(context.Background(), now.Add(90*time.Second)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expiry extended", err)
	}
}
func TestTURNRenewalCannotDowngradeOrReuseCredentials(t *testing.T) {
	issuer, _ := turncred.New(turncred.Config{URLs: []string{"turn:127.0.0.1:3478?transport=udp"}, Secret: strings.Repeat("k", 40), RelayOnly: true})
	for _, invalid := range []string{"disabled", "reused"} {
		t.Run(invalid, func(t *testing.T) {
			now := time.Now()
			m := &renewingMedia{&recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{}}}}
			m.connected.Store(true)
			r := newSignalRecovery(true, m)
			c, _ := issuer.Issue(now.Add(-turncred.Lifetime + 2*time.Minute))
			r.expiry = *c.ExpiresAt
			if invalid == "disabled" {
				c = turncred.Credentials{}
			}
			r.refreshICE = func(context.Context) (turncred.Credentials, error) { return c, nil }
			if err := r.tick(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			result := <-r.offers
			if !errors.Is(result.err, turncred.ErrConfig) || m.offersMade.Load() != 0 {
				t.Fatal("invalid credentials mutated peer")
			}
		})
	}
}

func TestTURNExpiryGuardAlsoAppliesBeforeAnswer(t *testing.T) {
	hub, phone, tokens, _, _ := renewableSignalClients(t)
	cfg := SignalCall{Config: testConfig(), ModuleID: "module", Display: "test", EnableICERestart: true}
	cfg.RecoverTransport = true
	cfg.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 7, InstallationID: tokens.Principal.ID}
	cfg.Route.TargetInstallationID = tokens.Principal.ID
	// Exercise the policy boundary without waiting for a real four-hour lease.
	cfg.ICEExpiresAt = time.Now().Add(30200 * time.Millisecond)
	cfg.RefreshICE = func(context.Context) (turncred.Credentials, error) {
		t.Error("renewal before connected")
		return turncred.Credentials{}, errors.New("unexpected")
	}
	b := &testBackend{}
	m := &recoveryMedia{signalTestMedia: &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: cfg.Binding, candidates: make(chan remotemedia.Candidate)}}
	done := make(chan Snapshot, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		snapshot, _ := RunSignaledCall(ctx, hub, cfg, func(ctx context.Context) (*Session, SignalMedia, error) {
			s, err := New(ctx, cfg.Config, b, func(context.Context) (Media, error) { return m, nil })
			return s, m, err
		})
		done <- snapshot
	}()
	signalRead(t, phone, "invite", "")
	select {
	case snapshot := <-done:
		if snapshot.EndReason != "turn_credentials_expired" || b.answers.Load() != 0 {
			t.Fatal(snapshot)
		}
	case <-ctx.Done():
		t.Fatal("ringing call escaped TURN expiry")
	}
}
