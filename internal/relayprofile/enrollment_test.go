//go:build cgo && (darwin || linux)

package relayprofile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

func approvedFile(t *testing.T, f *fixture, name string) identity.EnrollmentAuthorization {
	t.Helper()
	ctx := context.Background()
	r, err := f.store.BeginEnrollment(ctx, name, f.server.URL, "My Mac", []identity.ModuleChoice{{ID: "module", Alias: "Xiaomi"}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.store.SubmitEnrollment(ctx, name)
	if err != nil || status.Request.ID != r.ID {
		t.Fatal(status, err)
	}
	if err = f.remote.AdminApproveEnrollment(ctx, "tenant", r.ID, []string{"module"}); err != nil {
		t.Fatal(err)
	}
	file, err := f.remote.AdminIssueEnrollment(ctx, "tenant", r.ID, f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
func TestWebEnrollmentRoundTripReusesDurableKey(t *testing.T) {
	f := setup(t, nil)
	file := approvedFile(t, f, "web-profile")
	ctx := context.Background()
	before, err := f.store.load("web-profile")
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.restart().EnrollmentRequest("web-profile")
	if err != nil || request.ID != file.RequestID {
		t.Fatal(request, err)
	}
	bad := file
	bad.PublicKeyFingerprint = strings.Repeat("0", 64)
	if err = f.store.CompleteEnrollment(ctx, "web-profile", bad); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err = f.restart().CompleteEnrollment(ctx, "web-profile", file); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.load("web-profile")
	if err != nil || next.Phase != "ready" || string(next.Seed) != string(before.Seed) {
		t.Fatal(next.Phase, err)
	}
	if err = f.store.CompleteEnrollment(ctx, "web-profile", file); !errors.Is(err, ErrRecovery) {
		t.Fatal("automatic redemption retry", err)
	}
	if _, err = f.store.BeginEnrollment(ctx, "web-profile", f.server.URL, "Another", request.Modules); !errors.Is(err, ErrExists) {
		t.Fatal("overwrote profile", err)
	}
	config, err := f.store.ConnectionConfig(ctx, "web-profile")
	if err != nil || config.IdentityID == "" {
		t.Fatal(err)
	}
}
func TestEnrollmentPersistenceFailureKeepsUncertainPhase(t *testing.T) {
	f := setup(t, nil)
	file := approvedFile(t, f, "web-profile")
	ctx := context.Background()
	f.vault.fail = func(r record) bool { return r.Phase == "ready" }
	if err := f.store.CompleteEnrollment(ctx, "web-profile", file); !errors.Is(err, ErrVault) {
		t.Fatal(err)
	}
	state, err := f.restart().Status("web-profile")
	if err != nil || state.Phase != "enrolling" {
		t.Fatal(state, err)
	}
	if err = f.store.CompleteEnrollment(ctx, "web-profile", file); !errors.Is(err, ErrRecovery) {
		t.Fatal("ambiguous consumption retried", err)
	}
	list, err := f.remote.AdminEnrollmentRequests(ctx, "tenant")
	if err != nil || len(list) != 1 || list[0].State != "completed" {
		t.Fatal(list, err)
	}
}
func TestPersistenceBeforeConsumptionAllowsSafeRetry(t *testing.T) {
	f := setup(t, nil)
	file := approvedFile(t, f, "web-profile")
	ctx := context.Background()
	f.vault.fail = func(r record) bool { return r.Phase == "enrolling" }
	if err := f.store.CompleteEnrollment(ctx, "web-profile", file); !errors.Is(err, ErrVault) {
		t.Fatal(err)
	}
	f.vault.fail = nil
	if err := f.store.CompleteEnrollment(ctx, "web-profile", file); err != nil {
		t.Fatal("grant consumed before durable marker", err)
	}
}
