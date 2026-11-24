//go:build cgo

package identity

import (
	"context"
	"github.com/google/uuid"
	"github.com/iniwex5/vohive/internal/smsout"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func outgoingRequest(s *Store) smsout.Request {
	return smsout.Request{ID: uuid.NewString(), ModuleID: "module-a", Phone: "10086", Message: "测试短信🙂", CreatedAt: s.now()}
}

func TestSMSDurableClaimIdempotencyAndResultRecovery(t *testing.T) {
	s, dir, _ := testStore(t)
	ctx := context.Background()
	hub, _ := testHub(t, s, "tenant")
	phone, _ := testPhone(t, s, hub)
	r := outgoingRequest(s)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, err := s.SubmitSMS(ctx, phone.AccessToken, r)
			if err != nil || result.State != "queued" {
				t.Errorf("submit: %v %+v", err, result)
			}
		}()
	}
	workers.Wait()
	var claimed atomic.Int32
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request, err := s.ClaimSMS(ctx, hub.AccessToken)
			if err != nil {
				t.Error(err)
			}
			if request != nil {
				claimed.Add(1)
				if request.InstallationID != phone.Principal.ID {
					t.Error("unbound installation")
				}
			}
		}()
	}
	workers.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("hardware claims = %d", claimed.Load())
	}
	s.Close()
	next, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if request, err := next.ClaimSMS(ctx, hub.AccessToken); err != nil || request != nil {
		t.Fatal("restart replayed sending request", err)
	}
	if result, err := next.SubmitSMS(ctx, phone.AccessToken, r); err != nil || result.State != "sending" {
		t.Fatal(result, err)
	}
	result := smsout.Result{ID: r.ID, InstallationID: phone.Principal.ID, State: "sent", Segments: 1}
	for range 2 {
		if err := next.FinishSMS(ctx, hub.AccessToken, result); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := next.SubmitSMS(ctx, phone.AccessToken, r); err != nil || got != result {
		t.Fatal(got, err)
	}
	var content, number string
	if err := next.db.QueryRow(`SELECT message,phone FROM sms_outgoing WHERE id=?`, r.ID).Scan(&content, &number); err != nil || content != "" || number != "" {
		t.Fatal("completed content retained", err)
	}
	altered := r
	altered.Message = "different"
	if _, err := next.SubmitSMS(ctx, phone.AccessToken, altered); err != ErrInvalid {
		t.Fatal("reused ID changed payload", err)
	}
}

func TestSMSAuthorizationRevocationExpiryAndUnknown(t *testing.T) {
	s, _, clock := testStore(t)
	ctx := context.Background()
	hub, _ := testHub(t, s, "one")
	phone, _ := testPhone(t, s, hub)
	other, _ := testHub(t, s, "other")
	r := outgoingRequest(s)
	if _, err := s.SubmitSMS(ctx, hub.AccessToken, r); err != ErrUnauthorized {
		t.Fatal("hub submitted phone work", err)
	}
	bad := r
	bad.ModuleID = "module-b"
	if _, err := s.SubmitSMS(ctx, phone.AccessToken, bad); err != ErrUnauthorized {
		t.Fatal("foreign module accepted", err)
	}
	r.InstallationID = "forged"
	if _, err := s.SubmitSMS(ctx, phone.AccessToken, r); err != nil {
		t.Fatal(err)
	}
	if request, err := s.ClaimSMS(ctx, other.AccessToken); err != nil || request != nil {
		t.Fatal("cross hub claim", err)
	}
	if _, err := s.ClaimSMS(ctx, phone.AccessToken); err != ErrUnauthorized {
		t.Fatal("phone claimed work", err)
	}
	clock.Add(121)
	result, err := s.SubmitSMS(ctx, phone.AccessToken, r)
	if err != nil || result.State != "failed" || result.Code != "request_expired" {
		t.Fatal(result, err)
	}
	r2 := outgoingRequest(s)
	if _, err := s.SubmitSMS(ctx, phone.AccessToken, r2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSMS(ctx, hub.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE sms_outgoing SET updated=? WHERE id=?`, s.now().Add(-31*time.Minute).Unix(), r2.ID); err != nil {
		t.Fatal(err)
	}
	result, err = s.SubmitSMS(ctx, phone.AccessToken, r2)
	if err != nil || result.State != "unknown" {
		t.Fatal(result, err)
	}
	if request, err := s.ClaimSMS(ctx, hub.AccessToken); err != nil || request != nil {
		t.Fatal("uncertain message replayed", err)
	}
	r3 := outgoingRequest(s)
	if _, err := s.SubmitSMS(ctx, phone.AccessToken, r3); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInstallation(ctx, hub.AccessToken, phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if request, err := s.ClaimSMS(ctx, hub.AccessToken); err != nil || request != nil {
		t.Fatal("revoked phone dispatched", err)
	}
	if _, err := s.AdminDeleteRevokedTenant(ctx, "one", phone.Principal.ID); err != nil {
		t.Fatal("outbox prevented deletion", err)
	}
}
