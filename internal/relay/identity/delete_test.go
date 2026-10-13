//go:build cgo

package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDeleteRevokedPhoneCleansCredentialsAndPreservesHub(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	hub, _ := testHub(t, s, "home")
	phone, key := testPhone(t, s, hub)
	sibling, _ := testPhone(t, s, hub)
	if _, err := s.AdminDeleteRevokedTenant(ctx, "home", phone.Principal.ID); !errors.Is(err, ErrInvalid) {
		t.Fatal("active identity deletable", err)
	}
	rotated, err := s.Refresh(ctx, refreshProof(t, s, phone, key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Challenge(ctx, rotated.Principal.ID, rotated.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err = s.SetPushEndpoint(ctx, rotated.AccessToken, "sandbox", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminRevokeTenant(ctx, "home", phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminDeleteRevokedTenant(ctx, "other", phone.Principal.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("cross tenant deletion", err)
	}
	count, err := s.AdminDeleteRevokedTenant(ctx, "home", phone.Principal.ID)
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, table := range []string{"access_tokens", "refresh_used", "challenges", "push_endpoints", "identity_metadata", "identities"} {
		column := "identity_id"
		if table == "identity_metadata" || table == "identities" {
			column = "id"
		}
		var n int
		if err = s.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+column+"=?", phone.Principal.ID).Scan(&n); err != nil || n != 0 {
			t.Fatal("retained credential", table, n, err)
		}
	}
	for _, token := range []string{phone.AccessToken, rotated.AccessToken} {
		if _, err = s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("deleted credential usable", err)
		}
	}
	for _, token := range []string{hub.AccessToken, sibling.AccessToken} {
		if _, err = s.Authenticate(ctx, token); err != nil {
			t.Fatal("unrelated identity affected", err)
		}
	}
	grants, err := s.ManagedPairings(ctx, hub.AccessToken)
	if err != nil || len(grants) != 2 {
		t.Fatal("pairing history lost", grants, err)
	}
	items, err := s.AdminIdentities(ctx, "home")
	if err != nil || len(items) != 2 {
		t.Fatal("deleted phone still listed", items, err)
	}
}

func TestDeleteRevokedHubIncludesDescendantsAndInvalidatesInvites(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	hub, _ := testHub(t, s, "home")
	phone, _ := testPhone(t, s, hub)
	_, _ = testPhone(t, s, hub)
	other, _ := testHub(t, s, "other")
	grant, err := s.CreatePairing(ctx, hub.AccessToken, []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := proofFor(t, "pairing", grant)
	if err = s.AdminRevokeTenant(ctx, "home", hub.Principal.ID); err != nil {
		t.Fatal(err)
	}
	count, err := s.AdminDeleteRevokedTenant(ctx, "home", hub.Principal.ID)
	if err != nil || count != 3 {
		t.Fatal(count, err)
	}
	items, err := s.AdminIdentities(ctx, "home")
	if err != nil || len(items) != 0 {
		t.Fatal("Hub or descendants still listed", items, err)
	}
	if _, err = s.Redeem(ctx, proof); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted Hub invite redeemed", err)
	}
	if _, err = s.Authenticate(ctx, phone.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("descendant credentials usable", err)
	}
	if _, err = s.Authenticate(ctx, other.AccessToken); err != nil {
		t.Fatal("other tenant affected", err)
	}
	rows, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("orphaned foreign keys", rows.Err())
	}
}

func TestDeleteFailureRollsBackCredentialsAndIdentity(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	hub, _ := testHub(t, s, "home")
	_, _ = testPhone(t, s, hub)
	if err := s.AdminRevokeTenant(ctx, "home", hub.Principal.ID); err != nil {
		t.Fatal(err)
	}
	tables := []string{"identities", "identity_metadata", "access_tokens", "grants", "grant_metadata"}
	before := make(map[string]int)
	for _, table := range tables {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		before[table] = n
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_identity_delete BEFORE DELETE ON identities BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminDeleteRevokedTenant(ctx, "home", hub.Principal.ID); err == nil {
		t.Fatal("deletion failure not reported")
	}
	for _, table := range tables {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != before[table] {
			t.Fatal("partially deleted records after rollback", table, n, before[table], err)
		}
	}
}
