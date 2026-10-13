//go:build cgo

package relayadmin

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relaystatus"
)

func TestRuntimeOverviewAndPresenceStayTenantScopedAndExpire(t *testing.T) {
	now := time.Now().UTC()
	snapshot := relaystatus.Snapshot{Version: 1, ObservedAt: now, Running: true, IdentityHealthy: true, Build: "observed-relay"}
	f := adminFixtureRuntime(t, func() relaystatus.Observation { return relaystatus.Observe(snapshot, time.Now()) })
	enroll := func(tenant string) identity.Tokens {
		g, err := f.store.ProvisionEnrollment(context.Background(), tenant, []string{"module"})
		if err != nil {
			t.Fatal(err)
		}
		pub, key, _ := ed25519.GenerateKey(nil)
		proof := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
		proof.Signature = ed25519.Sign(key, identity.GrantMessage("enrollment", proof))
		token, err := f.store.Enroll(context.Background(), proof)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	owned, other := enroll("home"), enroll("other")
	snapshot.OnlineIdentities = []string{owned.Principal.ID, other.Principal.ID}
	snapshot.OnlineCount = 2
	if f.request("GET", "/admin/api/overview", nil, false).Code != 401 {
		t.Fatal("public runtime overview")
	}
	w := f.request("GET", "/admin/api/overview", nil, true)
	var overview struct {
		Runtime relaystatus.Observation `json:"runtime"`
	}
	if json.Unmarshal(w.Body.Bytes(), &overview) != nil || overview.Runtime.Snapshot.OnlineCount != 1 || len(overview.Runtime.Snapshot.OnlineIdentities) != 0 || strings.Contains(w.Body.String(), other.Principal.ID) {
		t.Fatal("cross-tenant runtime metadata")
	}
	w = f.request("GET", "/admin/api/identities", nil, true)
	if strings.Contains(w.Body.String(), other.Principal.ID) || !strings.Contains(w.Body.String(), `"signal_state":"online"`) {
		t.Fatal("presence scope")
	}
	snapshot.ObservedAt = now.Add(-9 * time.Second)
	w = f.request("GET", "/admin/api/identities", nil, true)
	if !strings.Contains(w.Body.String(), `"signal_state":"unknown"`) || strings.Contains(w.Body.String(), `"signal_state":"online"`) {
		t.Fatal("stale runtime inferred online")
	}
	snapshot.ObservedAt, snapshot.Running = time.Now().UTC(), false
	w = f.request("GET", "/admin/api/identities", nil, true)
	if !strings.Contains(w.Body.String(), `"signal_state":"offline"`) {
		t.Fatal("stopped runtime retained online")
	}
}
