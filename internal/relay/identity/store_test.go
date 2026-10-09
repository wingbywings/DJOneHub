//go:build cgo

package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string, *atomic.Int64) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "state")
	s, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	clock := &atomic.Int64{}
	clock.Store(time.Now().Unix())
	s.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	return s, directory, clock
}
func proofFor(t *testing.T, kind string, g Grant) (GrantProof, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	p := GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
	p.Signature = ed25519.Sign(key, GrantMessage(kind, p))
	return p, key
}
func testHub(t *testing.T, s *Store, tenant string) (Tokens, ed25519.PrivateKey) {
	t.Helper()
	g, err := s.ProvisionEnrollment(context.Background(), tenant, []string{"module-a", "module-b"})
	if err != nil {
		t.Fatal(err)
	}
	p, key := proofFor(t, "enrollment", g)
	tokens, err := s.Enroll(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return tokens, key
}
func testPhone(t *testing.T, s *Store, hub Tokens) (Tokens, ed25519.PrivateKey) {
	t.Helper()
	g, err := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	p, key := proofFor(t, "pairing", g)
	tokens, err := s.Redeem(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return tokens, key
}
func refreshProof(t *testing.T, s *Store, tokens Tokens, key ed25519.PrivateKey) RefreshProof {
	t.Helper()
	c, err := s.Challenge(context.Background(), tokens.Principal.ID, tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	p := RefreshProof{IdentityID: tokens.Principal.ID, RefreshToken: tokens.RefreshToken, ChallengeID: c.ID, Challenge: c.Value}
	p.Signature = ed25519.Sign(key, RefreshMessage(p))
	return p
}

func TestTenantHubAndModuleIsolation(t *testing.T) {
	s, _, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant-a")
	other, _ := testHub(t, s, "tenant-b")
	sibling, _ := testHub(t, s, "tenant-a")
	phone, _ := testPhone(t, s, hub)
	ctx := context.Background()
	if _, err := s.AuthorizeModule(ctx, phone.AccessToken, hub.Principal.ID, "module-a"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ hub, module string }{{hub.Principal.ID, "module-b"}, {other.Principal.ID, "module-a"}, {sibling.Principal.ID, "module-a"}} {
		if _, err := s.AuthorizeModule(ctx, phone.AccessToken, target.hub, target.module); !errors.Is(err, ErrUnauthorized) {
			t.Fatal(err)
		}
	}
	if _, err := s.CreatePairing(ctx, hub.AccessToken, []string{"unowned"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.CreatePairing(ctx, phone.AccessToken, []string{"module-a"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	for _, token := range []string{other.AccessToken, sibling.AccessToken, phone.AccessToken} {
		if err := s.RevokeInstallation(ctx, token, phone.Principal.ID); !errors.Is(err, ErrUnauthorized) {
			t.Fatal(err)
		}
	}
}
func TestConcurrentPairingRedemptionAcrossConnections(t *testing.T) {
	s, dir, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	g, err := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := proofFor(t, "pairing", g)
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			selected := s
			if i%2 == 1 {
				selected = second
			}
			_, err := selected.Redeem(context.Background(), p)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrUnauthorized) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("grant redeemed more than once", winners.Load())
	}
}
func TestBadProofDoesNotConsumeGrant(t *testing.T) {
	s, _, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	g, _ := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module-a"})
	p, key := proofFor(t, "pairing", g)
	bad := p
	bad.Secret = randomToken(32)
	bad.Signature = ed25519.Sign(key, GrantMessage("pairing", bad))
	if _, err := s.Redeem(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	bad = p
	bad.Signature = ed25519.Sign(key, GrantMessage("enrollment", bad))
	if _, err := s.Redeem(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.Redeem(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}
func TestGrantAndAccessExpiration(t *testing.T) {
	s, _, clock := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	lease, cancel, err := s.AuthorizeContext(context.Background(), hub.AccessToken, hub.Principal.ID, "module-a")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	g, _ := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module-a"})
	p, _ := proofFor(t, "pairing", g)
	clock.Add(301)
	if _, err := s.Redeem(context.Background(), p); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	clock.Add(600)
	if _, err := s.Authenticate(context.Background(), hub.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	select {
	case <-lease.Done():
		if !errors.Is(context.Cause(lease), ErrUnauthorized) {
			t.Fatal("unexpected expiration cause", context.Cause(lease))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expired access token kept authorization alive")
	}
}
func TestRefreshProofRotationAndSignedReuseRevocation(t *testing.T) {
	s, _, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	phone, key := testPhone(t, s, hub)
	p := refreshProof(t, s, phone, key)
	bad := p
	bad.Challenge = randomToken(32)
	bad.Signature = ed25519.Sign(key, RefreshMessage(bad))
	if _, err := s.Refresh(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	_, wrongKey, _ := ed25519.GenerateKey(nil)
	bad = p
	bad.Signature = ed25519.Sign(wrongKey, RefreshMessage(bad))
	if _, err := s.Refresh(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	rotated, err := s.Refresh(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RefreshToken == phone.RefreshToken || rotated.AccessToken == phone.AccessToken {
		t.Fatal("tokens not rotated")
	}
	if _, err := s.Refresh(context.Background(), p); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("challenge replay accepted", err)
	}
	if _, err := s.Authenticate(context.Background(), rotated.AccessToken); err != nil {
		t.Fatal("identical transport retry revoked fresh identity", err)
	}
	lease, stop, err := s.AuthorizeContext(context.Background(), rotated.AccessToken, hub.Principal.ID, "module-a")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	reuse := refreshProof(t, s, phone, key)
	if _, err := s.Refresh(context.Background(), reuse); !errors.Is(err, ErrRefreshReuse) {
		t.Fatal(err)
	}
	select {
	case <-lease.Done():
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel authorization")
	}
	if _, err := s.Authenticate(context.Background(), rotated.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}
func TestRefreshChallengeBoundedAndExpired(t *testing.T) {
	s, _, clock := testStore(t)
	hub, key := testHub(t, s, "tenant")
	p := refreshProof(t, s, hub, key)
	for i := 0; i < 7; i++ {
		if _, err := s.Challenge(context.Background(), hub.Principal.ID, hub.RefreshToken); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Challenge(context.Background(), hub.Principal.ID, hub.RefreshToken); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	clock.Add(61)
	if _, err := s.Refresh(context.Background(), p); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.Challenge(context.Background(), hub.Principal.ID, hub.RefreshToken); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(RefreshLifetime / time.Second))
	if _, err := s.Challenge(context.Background(), hub.Principal.ID, hub.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}
func TestTransactionFailureDoesNotBurnPairing(t *testing.T) {
	s, _, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	g, _ := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module-a"})
	p, _ := proofFor(t, "pairing", g)
	if _, err := s.db.Exec("CREATE TRIGGER fail_access BEFORE INSERT ON access_tokens BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem(context.Background(), p); err == nil {
		t.Fatal("fault not injected")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_access"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem(context.Background(), p); err != nil {
		t.Fatal("partial transaction consumed pairing", err)
	}
}
func TestRestartRetainsRevocationsWithoutPlaintextSecrets(t *testing.T) {
	s, dir, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	phone, _ := testPhone(t, s, hub)
	if err := s.RevokeInstallation(context.Background(), hub.AccessToken, phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInstallation(context.Background(), hub.AccessToken, phone.Principal.ID); err != nil {
		t.Fatal("revoke not idempotent", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "identities.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{hub.RefreshToken, hub.AccessToken, phone.RefreshToken, phone.AccessToken} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("plaintext credential persisted")
		}
	}
	info, _ := os.Stat(filepath.Join(dir, "identities.sqlite"))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Authenticate(context.Background(), phone.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := reopened.Authenticate(context.Background(), hub.AccessToken); err != nil {
		t.Fatal(err)
	}
}
func TestHubRevocationInvalidatesChildrenAndPendingGrants(t *testing.T) {
	s, _, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	phone, _ := testPhone(t, s, hub)
	g, _ := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module-a"})
	p, _ := proofFor(t, "pairing", g)
	if err := s.AdminRevoke(context.Background(), hub.Principal.ID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{hub.AccessToken, phone.AccessToken} {
		if _, err := s.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
			t.Fatal(err)
		}
	}
	if _, err := s.Redeem(context.Background(), p); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}
func TestExternalRevocationCancelsAuthorizationLease(t *testing.T) {
	s, dir, _ := testStore(t)
	hub, _ := testHub(t, s, "tenant")
	phone, _ := testPhone(t, s, hub)
	lease, stop, err := s.AuthorizeContext(context.Background(), phone.AccessToken, hub.Principal.ID, "module-a")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.AdminRevoke(context.Background(), phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("external revocation ignored")
	}
}

func TestSharedSwiftSigningFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/proof-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]string
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(f["seed_hex"])
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(seed)
	p := GrantProof{ID: f["grant_id"], Secret: f["grant_secret"], PublicKey: key.Public().(ed25519.PublicKey)}
	if string(GrantMessage("pairing", p)) != f["grant_message"] || base64.StdEncoding.EncodeToString(ed25519.Sign(key, GrantMessage("pairing", p))) != f["grant_signature"] {
		t.Fatal("grant fixture mismatch")
	}
	r := RefreshProof{IdentityID: f["identity_id"], RefreshToken: f["refresh_token"], ChallengeID: f["challenge_id"], Challenge: f["challenge"]}
	if string(RefreshMessage(r)) != f["refresh_message"] || base64.StdEncoding.EncodeToString(ed25519.Sign(key, RefreshMessage(r))) != f["refresh_signature"] {
		t.Fatal("refresh fixture mismatch")
	}
	// CryptoKit may randomize valid Ed25519 signatures. Verify actual captured
	// Swift outputs instead of requiring byte equality with Go's signatures.
	grantSig, err := base64.StdEncoding.DecodeString(f["swift_grant_signature"])
	if err != nil {
		t.Fatal(err)
	}
	refreshSig, err := base64.StdEncoding.DecodeString(f["swift_refresh_signature"])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(p.PublicKey, GrantMessage("pairing", p), grantSig) || !ed25519.Verify(p.PublicKey, RefreshMessage(r), refreshSig) {
		t.Fatal("Swift signatures do not verify in Go")
	}
}
