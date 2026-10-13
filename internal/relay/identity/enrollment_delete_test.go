//go:build cgo

package identity

import (
	"context"
	"errors"
	"testing"
)

func TestDeleteEnrollmentAcrossStatesPreservesRegisteredIdentity(t *testing.T) {
	for _, state := range []string{"pending", "approved", "rejected", "expired", "completed"} {
		t.Run(state, func(t *testing.T) {
			s, _, clock := testStore(t)
			ctx := context.Background()
			r, key := enrollmentFixture(t, s)
			if _, err := s.SubmitEnrollmentRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
			var file EnrollmentAuthorization
			var tokens Tokens
			if state != "pending" {
				if err := s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"module-a"}); err != nil {
					t.Fatal(err)
				}
				var err error
				file, err = s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.AdminDeleteEnrollment(ctx, "other", r.ID); !errors.Is(err, ErrUnauthorized) {
					t.Fatal("cross tenant", err)
				}
			}
			if state == "completed" {
				var err error
				tokens, err = s.Enroll(ctx, boundProof(file.Grant, key))
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == "rejected" {
				if err := s.AdminRejectEnrollment(ctx, "home", r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if state == "expired" {
				clock.Add(31 * 60)
			}
			count, err := s.AdminDeleteEnrollment(ctx, "home", r.ID)
			if err != nil || (state != "pending" && count != 1) {
				t.Fatal(count, err)
			}
			items, err := s.AdminEnrollmentRequests(ctx, "home")
			if err != nil || len(items) != 0 {
				t.Fatal("request still listed", items, err)
			}
			if state != "pending" {
				if _, err := s.Enroll(ctx, boundProof(file.Grant, key)); !errors.Is(err, ErrUnauthorized) {
					t.Fatal("old file usable", err)
				}
			}
			if state == "completed" {
				if _, err := s.Authenticate(ctx, tokens.AccessToken); err != nil {
					t.Fatal("registered Hub affected", err)
				}
			}
		})
	}
}

func TestDeleteAuthorizationInvalidatesOnlySelectedFile(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	r, key := enrollmentFixture(t, s)
	if _, err := s.SubmitEnrollmentRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"module-a"}); err != nil {
		t.Fatal(err)
	}
	old, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteEnrollmentAuthorization(ctx, "other", r.ID, latest.Grant.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("cross tenant", err)
	}
	if err = s.AdminDeleteEnrollmentAuthorization(ctx, "home", r.ID, latest.Grant.ID); err != nil {
		t.Fatal(err)
	}
	for _, file := range []EnrollmentAuthorization{old, latest} {
		if _, err = s.Enroll(ctx, boundProof(file.Grant, key)); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("old authorization revived", err)
		}
	}
	items, err := s.AdminEnrollmentRequests(ctx, "home")
	if err != nil || len(items) != 1 || items[0].State != "approved" || items[0].Authorization.ID != old.Grant.ID {
		t.Fatal(items, err)
	}
	fresh, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal("cannot issue fresh authorization", err)
	}
	tokens, err := s.Enroll(ctx, boundProof(fresh.Grant, key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteEnrollmentAuthorization(ctx, "home", r.ID, fresh.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, tokens.AccessToken); err != nil {
		t.Fatal("consumed file deletion revoked identity", err)
	}
	if _, err = s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL); !errors.Is(err, ErrInvalid) {
		t.Fatal("completed request reopened", err)
	}
}

func TestDeleteEnrollmentRollbackRestoresDownloadedAuthorization(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	r, key := enrollmentFixture(t, s)
	if _, err := s.SubmitEnrollmentRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminApproveEnrollment(ctx, "home", r.ID, []string{"module-a"}); err != nil {
		t.Fatal(err)
	}
	file, err := s.AdminIssueEnrollment(ctx, "home", r.ID, r.RelayURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_request_delete BEFORE DELETE ON enrollment_requests BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminDeleteEnrollment(ctx, "home", r.ID); err == nil {
		t.Fatal("failure not surfaced")
	}
	if _, err = s.Enroll(ctx, boundProof(file.Grant, key)); err != nil {
		t.Fatal("partial deletion after rollback", err)
	}
}
