//go:build cgo

package identity

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPushEndpointScopeRotationRevocationAndMigration(t *testing.T) {
	s, directory, _ := testStore(t)
	ctx := context.Background()
	hub, _ := testHub(t, s, "tenant")
	phone, _ := testPhone(t, s, hub)
	other, _ := testHub(t, s, "other")
	token := strings.Repeat("a", 64)
	if err := s.SetPushEndpoint(ctx, hub.AccessToken, "sandbox", token); err != ErrUnauthorized {
		t.Fatal("hub registered address", err)
	}
	if err := s.SetPushEndpoint(ctx, phone.AccessToken, "sandbox", token); err != nil {
		t.Fatal(err)
	}
	e, err := s.PushEndpoint(ctx, hub.AccessToken, phone.Principal.ID, "module-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][2]string{{other.AccessToken, "module-a"}, {hub.AccessToken, "module-b"}, {phone.AccessToken, "module-a"}} {
		if _, err := s.PushEndpoint(ctx, args[0], phone.Principal.ID, args[1]); err == nil {
			t.Fatal("scope bypass")
		}
	}
	if err := s.SetPushEndpoint(ctx, phone.AccessToken, "sandbox", token); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidatePushEndpoint(ctx, e, time.Time{}); err != nil {
		t.Fatal(err)
	}
	newer, err := s.PushEndpoint(ctx, hub.AccessToken, phone.Principal.ID, "module-a")
	if err != nil {
		t.Fatal("old APNs callback erased new registration")
	}
	s.InvalidatePushEndpoint(ctx, newer, newer.Updated.Add(-time.Second))
	if _, err := s.PushEndpoint(ctx, hub.AccessToken, phone.Principal.ID, "module-a"); err != nil {
		t.Fatal("old APNs timestamp erased new registration")
	}
	if err := s.RevokeInstallation(ctx, hub.AccessToken, phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PushEndpoint(ctx, hub.AccessToken, phone.Principal.ID, "module-a"); err == nil {
		t.Fatal("revoked target routable")
	}
	// Reproduce an existing schema v1 database, then reopen with this migration.
	if _, err := s.db.Exec("DROP TABLE push_endpoints; PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	next, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := next.Authenticate(ctx, hub.AccessToken); err != nil {
		t.Fatal("migration damaged identity")
	}
	var version int
	next.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 2 {
		t.Fatal(version)
	}
}
