//go:build cgo

package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func enrollmentFixture(t *testing.T, s *Store) (EnrollmentRequest, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := EnrollmentRequest{Version: 1, ID: "request_" + randomToken(16), Name: "Mac", RelayURL: "https://relay.test", PublicKey: pub, Modules: []ModuleChoice{{"module-a", "Xiaomi"}, {"module-b", "Xiaomi"}}, CreatedAt: s.now().UTC(), ExpiresAt: s.now().Add(30 * time.Minute).UTC()}
	r.Signature = ed25519.Sign(key, EnrollmentRequestMessage(r))
	return r, key
}
func boundProof(g Grant, key ed25519.PrivateKey) GrantProof {
	p := GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: key.Public().(ed25519.PublicKey)}
	p.Signature = ed25519.Sign(key, GrantMessage("enrollment", p))
	return p
}
func TestBoundEnrollmentApprovalReissueAndSingleUse(t *testing.T) {
	ctx := context.Background()
	s, _, _ := testStore(t)
	r, key := enrollmentFixture(t, s)
	result, err := s.SubmitEnrollmentRequest(ctx, r)
	if err != nil || result.State != "pending" {
		t.Fatal(result, err)
	}
	if _, err = s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL); err == nil {
		t.Fatal("unapproved request issued")
	}
	if err = s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"not-in-request"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err = s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"module-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminIssueEnrollment(ctx, "other", r.ID, r.RelayURL); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err = s.AdminIssueEnrollment(ctx, "home", r.ID, "https://wrong.test"); err == nil {
		t.Fatal("wrong origin")
	}
	first, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enroll(ctx, boundProof(first.Grant, key)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old grant still active", err)
	}
	bad, _ := proofFor(t, "enrollment", second.Grant)
	if _, err = s.Enroll(ctx, bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("unbound key accepted", err)
	}
	tokens, err := s.Enroll(ctx, boundProof(second.Grant, key))
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Principal.Modules[0] != "module-a" || len(tokens.Principal.Modules) != 1 {
		t.Fatal(tokens.Principal)
	}
	if _, err = s.Enroll(ctx, boundProof(second.Grant, key)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("consumed twice", err)
	}
	if _, err = s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL); err == nil {
		t.Fatal("completed request re-issued")
	}
	list, err := s.AdminEnrollmentRequests(ctx, "home")
	if err != nil || len(list) != 1 || list[0].State != "completed" || list[0].HubID != tokens.Principal.ID {
		t.Fatal(list, err)
	}
	identities, err := s.AdminIdentities(ctx, "home")
	if err != nil || len(identities) != 1 || identities[0].Name != "Mac" {
		t.Fatal(identities, err)
	}
}
func TestEnrollmentValidationAndTamperReplay(t *testing.T) {
	s, _, clock := testStore(t)
	r, key := enrollmentFixture(t, s)
	ctx := context.Background()
	altered := r
	altered.Modules = append([]ModuleChoice(nil), r.Modules...)
	altered.Modules[0].ID = "other"
	if _, err := s.SubmitEnrollmentRequest(ctx, altered); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.SubmitEnrollmentRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitEnrollmentRequest(ctx, r); err != nil {
		t.Fatal("exact replay failed", err)
	}
	altered.Name = "changed"
	altered.Signature = ed25519.Sign(key, EnrollmentRequestMessage(altered))
	if _, err := s.SubmitEnrollmentRequest(ctx, altered); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("ID replaced", err)
	}
	duplicate := r
	duplicate.ID = "request_" + randomToken(16)
	duplicate.Modules = []ModuleChoice{r.Modules[0], r.Modules[0]}
	duplicate.Signature = ed25519.Sign(key, EnrollmentRequestMessage(duplicate))
	if _, err := s.SubmitEnrollmentRequest(ctx, duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	clock.Add(31 * 60)
	if err := s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"module-a"}); err == nil {
		t.Fatal("expired approval")
	}
}
func TestConcurrentReissueVersusRedemptionAcrossProcesses(t *testing.T) {
	s, dir, _ := testStore(t)
	r, key := enrollmentFixture(t, s)
	ctx := context.Background()
	s.SubmitEnrollmentRequest(ctx, r)
	s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"module-a"})
	a, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	var winners atomic.Int32
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := s.Enroll(ctx, boundProof(a.Grant, key)); err == nil {
			winners.Add(1)
		} else if !errors.Is(err, ErrUnauthorized) {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		next, err := other.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
		if err == nil {
			if _, err = other.Enroll(ctx, boundProof(next.Grant, key)); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrUnauthorized) {
				t.Error(err)
			}
		} else if !errors.Is(err, ErrInvalid) {
			t.Error(err)
		}
	}()
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("expected exactly one registration", winners.Load())
	}
}
func TestPairingCancelNamesAndResultAssociation(t *testing.T) {
	ctx := context.Background()
	s, _, _ := testStore(t)
	hub, _ := testHub(t, s, "home")
	other, _ := testHub(t, s, "home")
	g, err := s.CreatePairing(ctx, hub.AccessToken, []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.NamePairing(ctx, hub.AccessToken, g.ID, "My iPhone"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokePairing(ctx, other.AccessToken, g.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	p, _ := proofFor(t, "pairing", g)
	phone, err := s.Redeem(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.PairingStatus(ctx, hub.AccessToken, g.ID)
	if err != nil || status.IdentityID != phone.Principal.ID || status.State != "consumed" {
		t.Fatal(status, err)
	}
	phones, err := s.ManagedInstallations(ctx, hub.AccessToken)
	if err != nil || len(phones) != 1 || phones[0].Name != "My iPhone" {
		t.Fatal(phones, err)
	}
	wire, _ := json.Marshal(phones)
	if string(wire) == "null" {
		t.Fatal("nil list")
	}
	if err = s.NameInstallation(ctx, other.AccessToken, phone.Principal.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	cancel, err := s.CreatePairing(ctx, hub.AccessToken, []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokePairing(ctx, hub.AccessToken, cancel.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = proofFor(t, "pairing", cancel)
	if _, err = s.Redeem(ctx, p); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("cancelled QR worked", err)
	}
	if err = s.AdminRevokeTenant(ctx, "other", hub.Principal.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err = s.AdminRevokeTenant(ctx, "home", hub.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, phone.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}
func TestSchemaTwoMigrationPreservesCredentialsAndUnknownDates(t *testing.T) {
	ctx := context.Background()
	s, dir, _ := testStore(t)
	hub, _ := testHub(t, s, "home")
	phone, _ := testPhone(t, s, hub)
	if _, err := s.db.Exec("DROP TABLE grant_metadata; DROP TABLE enrollment_requests; DROP TABLE identity_metadata; PRAGMA user_version=2;"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	next, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err = next.Authenticate(ctx, phone.AccessToken); err != nil {
		t.Fatal(err)
	}
	result, err := next.ManagedInstallations(ctx, hub.AccessToken)
	if err != nil || len(result) != 1 || result[0].CreatedAt != nil {
		t.Fatal(result, err)
	}
	g, err := next.CreatePairing(ctx, hub.AccessToken, []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := proofFor(t, "pairing", g)
	if _, err = next.Redeem(ctx, p); err != nil {
		t.Fatal(err)
	}
}
