//go:build cgo

package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func (f *signalFixture) refresh(t *testing.T, old identity.Tokens) identity.Tokens {
	t.Helper()
	c, err := f.store.Challenge(context.Background(), old.Principal.ID, old.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	p := identity.RefreshProof{IdentityID: old.Principal.ID, RefreshToken: old.RefreshToken, ChallengeID: c.ID, Challenge: c.Value}
	p.Signature = ed25519.Sign(f.keys[old.Principal.ID], identity.RefreshMessage(p))
	next, err := f.store.Refresh(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func (f *signalFixture) expire(t *testing.T, token string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.directory, "identities.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash := sha256.Sum256([]byte(token))
	if _, err := db.Exec("UPDATE access_tokens SET expires=? WHERE hash=?", at.Unix(), hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
}
func TestWSSReauthenticationPreservesRouteAndRejectsOldExpiry(t *testing.T) {
	f := signalSetup(t)
	hub := f.dial(t, f.hub)
	phone := f.dial(t, f.phone)
	invite := invitation(hub, f.phone)
	sendSignal(t, hub, invite)
	receive(t, phone, "invite")
	receive(t, hub, "delivered")
	oldEpoch := hub.Epoch()
	nextHub, nextPhone := f.refresh(t, f.hub), f.refresh(t, f.phone)
	if _, err := hub.Renew(context.Background(), nextHub.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.Renew(context.Background(), nextPhone.AccessToken); err != nil {
		t.Fatal(err)
	}
	f.expire(t, f.hub.AccessToken, time.Now().Add(-time.Second))
	f.expire(t, f.phone.AccessToken, time.Now().Add(-time.Second))
	// Give obsolete watchers a full polling interval to expose cancellation races.
	select {
	case <-hub.Done():
		t.Fatal("old Hub watcher cancelled renewed socket")
	case <-phone.Done():
		t.Fatal("old phone watcher cancelled renewed socket")
	case <-time.After(1200 * time.Millisecond):
	}
	command := invite
	command.Type = "command"
	command.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	sendSignal(t, phone, command)
	receive(t, hub, "command")
	if hub.Epoch() != oldEpoch {
		t.Fatal("reauth changed call epoch")
	}
	if err := f.store.RevokeInstallation(context.Background(), nextHub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	receive(t, hub, "peer_closed")
}
func TestWSSReauthenticationCannotSwitchIdentity(t *testing.T) {
	f := signalSetup(t)
	hub := f.dial(t, f.hub)
	phone := f.dial(t, f.phone)
	m := invitation(hub, f.phone)
	sendSignal(t, hub, m)
	receive(t, hub, "delivered")
	receive(t, phone, "invite")
	sibling := f.enroll(t, "tenant")
	if _, err := hub.Renew(context.Background(), sibling.AccessToken); err == nil {
		t.Fatal("reauth switched Hub")
	}
	receive(t, phone, "peer_closed")
	if _, err := f.store.Authenticate(context.Background(), sibling.AccessToken); err != nil {
		t.Fatal("rejected swap revoked unrelated identity", err)
	}
}
func TestWSSExpiredAuthorizationCannotBeRevived(t *testing.T) {
	f := signalSetup(t)
	hub := f.dial(t, f.hub)
	next := f.refresh(t, f.hub)
	f.expire(t, f.hub.AccessToken, time.Now().Add(-time.Second))
	if _, err := hub.Renew(context.Background(), next.AccessToken); err == nil {
		t.Fatal("expired connection revived")
	}
	select {
	case <-hub.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("expired connection alive")
	}
}
func TestAutomaticCredentialRefreshAndSocketRenewal(t *testing.T) {
	f := signalSetup(t)
	f.expire(t, f.hub.AccessToken, time.Now().Add(10*time.Second))
	hub := f.dial(t, f.hub)
	phone := f.dial(t, f.phone)
	invite := invitation(hub, f.phone)
	sendSignal(t, hub, invite)
	receive(t, phone, "invite")
	receive(t, hub, "delivered")
	api, err := relayclient.New(f.http.URL, relayclient.Options{TLSConfig: f.tls})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	var saves atomic.Int32
	credentials, err := relayclient.NewCredentials(api, f.hub, f.keys[f.hub.Principal.ID], func(next identity.Tokens) error {
		if next.RefreshToken == f.hub.RefreshToken {
			return identity.ErrInvalid
		}
		saves.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- hub.MaintainAuthorization(ctx, credentials.RefreshAccess) }()
	deadline := time.Now().Add(3 * time.Second)
	for !hub.Identity().AccessExpiresAt.After(time.Now().Add(time.Minute)) {
		if time.Now().After(deadline) {
			t.Fatal("automatic renewal did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if saves.Load() != 1 {
		t.Fatal("credential save did not precede WSS renewal")
	}
	f.expire(t, f.hub.AccessToken, time.Now().Add(-time.Second))
	command := invite
	command.Type = "command"
	command.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	sendSignal(t, phone, command)
	receive(t, hub, "command")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("authorization worker leaked")
	}
}
