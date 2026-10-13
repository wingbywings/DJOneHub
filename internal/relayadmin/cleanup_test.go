//go:build cgo

package relayadmin

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

func TestAuditClearScopesAllCompletedRowsAndPreservesPending(t *testing.T) {
	f := adminFixture(t)
	ctx := context.Background()
	for n := 0; n < 300; n++ {
		id, err := f.server.audit.begin(ctx, "operator", "home", "fixture", "object")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.server.audit.finish(ctx, id, "success"); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := f.server.audit.begin(ctx, "operator", "home", "unfinished", "object")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.server.audit.begin(ctx, "other_operator", "other", "fixture", "object")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.audit.finish(ctx, foreign, "success"); err != nil {
		t.Fatal(err)
	}
	if w := f.request("POST", "/admin/api/audit/clear", []byte(`{}`), false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := f.request("POST", "/admin/api/audit/clear", []byte(`{}`), true)
	var response struct {
		Count int64 `json:"cleared_count"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Count != 300 {
		t.Fatal(w.Code, w.Body.String())
	}
	events, err := f.server.audit.list(ctx, "home")
	if err != nil || len(events) != 2 || events[0].Action != "clear_audit" || events[0].Result != "success" || events[1].ID != pending {
		t.Fatal(events, err)
	}
	other, err := f.server.audit.list(ctx, "other")
	if err != nil || len(other) != 1 || other[0].ID != foreign {
		t.Fatal("other tenant audit removed", other, err)
	}
	if err = f.server.audit.finish(ctx, pending, "success"); err != nil {
		t.Fatal(err)
	}
	events, err = f.server.audit.list(ctx, "home")
	if err != nil || events[1].Result != "success" {
		t.Fatal("in-flight operation cannot finish", events, err)
	}
}

func TestCleanupEndpointsRequireAuditAndRejectInvalidCSRF(t *testing.T) {
	f := adminFixture(t)
	r, _ := submitRequest(t, f)
	path := "/admin/api/requests/" + r.ID + "/delete"
	if w := f.request("POST", path, []byte(`{}`), false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	saved := f.csrf
	f.csrf = "invalid"
	for _, endpoint := range []string{path, "/admin/api/audit/clear"} {
		if w := f.request("POST", endpoint, []byte(`{}`), true); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	f.csrf = saved
	f.server.audit.db.Close()
	if w := f.request("POST", path, []byte(`{}`), true); w.Code != 503 {
		t.Fatal(w.Code)
	}
	items, err := f.store.AdminEnrollmentRequests(context.Background(), "home")
	if err != nil || len(items) != 1 {
		t.Fatal("request deleted without audit", items, err)
	}
}

func TestRequestDeleteEndpointRemovesRequestAndLogsOperation(t *testing.T) {
	f := adminFixture(t)
	r, _ := submitRequest(t, f)
	w := f.request("POST", "/admin/api/requests/"+r.ID+"/delete", []byte(`{}`), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	items, err := f.store.AdminEnrollmentRequests(context.Background(), "home")
	if err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	events, err := f.server.audit.list(context.Background(), "home")
	if err != nil || events[0].Action != "delete_enrollment_request" || events[0].Result != "success" {
		t.Fatal(events, err)
	}
}

func TestAuthorizationDeleteEndpointInvalidatesFileAndPreservesRequest(t *testing.T) {
	f := adminFixture(t)
	r, key := submitRequest(t, f)
	if w := f.request("POST", "/admin/api/requests/"+r.ID+"/approve", []byte(`{"modules":["module-a"]}`), true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := f.request("POST", "/admin/api/requests/"+r.ID+"/authorization", []byte(`{}`), true)
	var file identity.EnrollmentAuthorization
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &file) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/admin/api/requests/" + r.ID + "/authorizations/" + file.Grant.ID + "/delete"
	if w = f.request("POST", path, []byte(`{}`), false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w = f.request("POST", path, []byte(`{}`), true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	proof := identity.GrantProof{ID: file.Grant.ID, Secret: file.Grant.Secret, PublicKey: key.Public().(ed25519.PublicKey)}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("enrollment", proof))
	if _, err := f.store.Enroll(context.Background(), proof); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("deleted file usable", err)
	}
	items, err := f.store.AdminEnrollmentRequests(context.Background(), "home")
	if err != nil || len(items) != 1 || items[0].State != "approved" || items[0].Authorization != nil {
		t.Fatal(items, err)
	}
}
