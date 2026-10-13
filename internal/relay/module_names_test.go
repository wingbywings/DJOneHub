//go:build cgo

package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func TestModuleNamesAuthenticatedScopedAndUpdated(t *testing.T) {
	f := signalSetup(t)
	client, err := relayclient.New(f.http.URL, relayclient.Options{TLSConfig: f.tls})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	publish := func(token string, names []identity.ModuleChoice) error {
		return client.PublishModuleNames(context.Background(), token, names)
	}
	read := func(token string, status int) (bool, []identity.ModuleChoice) {
		t.Helper()
		req, _ := http.NewRequest("POST", f.http.URL+"/v1/module-names", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := f.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("read status %d want %d", res.StatusCode, status)
		}
		var body struct {
			Hub       string                  `json:"hub_id"`
			Available bool                    `json:"available"`
			Modules   []identity.ModuleChoice `json:"modules"`
		}
		if status == 200 {
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		return body.Available, body.Modules
	}
	if available, _ := read(f.phone.AccessToken, 200); available {
		t.Fatal("catalogue exists before publication")
	}
	good := []identity.ModuleChoice{{ID: "module", Alias: "移动主卡"}, {ID: "other", Alias: "备用卡"}}
	if err := publish(f.hub.AccessToken, good); err != nil {
		t.Fatal(err)
	}
	available, names := read(f.phone.AccessToken, 200)
	if !available || len(names) != 1 || names[0] != good[0] {
		t.Fatalf("phone must only see its authorized line: %+v", names)
	}
	for _, bad := range [][]identity.ModuleChoice{
		{{ID: "module", Alias: "覆盖"}},
		{{ID: "module", Alias: "覆盖"}, {ID: "module", Alias: "覆盖"}},
		{{ID: "module", Alias: "覆盖"}, {ID: "foreign", Alias: "越权"}},
		{{ID: "module", Alias: "坏\n名称"}, {ID: "other", Alias: "备用"}},
		{{ID: "module", Alias: "坏\u202e名称"}, {ID: "other", Alias: "备用"}},
	} {
		if err := publish(f.hub.AccessToken, bad); err == nil {
			t.Fatal("invalid publication accepted")
		}
	}
	if err := publish(f.phone.AccessToken, good); err == nil {
		t.Fatal("phone may not publish names")
	}
	_, names = read(f.phone.AccessToken, 200)
	if len(names) != 1 || names[0].Alias != "移动主卡" {
		t.Fatal("invalid publication changed existing names")
	}
	good[0].Alias = "新备注"
	if err := publish(f.hub.AccessToken, good); err != nil {
		t.Fatal(err)
	}
	_, names = read(f.phone.AccessToken, 200)
	if names[0].Alias != "新备注" {
		t.Fatal("existing phone did not receive rename")
	}
	other := f.enroll(t, "another-tenant")
	if err := publish(other.AccessToken, []identity.ModuleChoice{{ID: "module", Alias: "他人备注"}, {ID: "other", Alias: "他人备用"}}); err != nil {
		t.Fatal(err)
	}
	_, names = read(f.phone.AccessToken, 200)
	if names[0].Alias != "新备注" {
		t.Fatal("cross-hub name leak")
	}
	read("", 401)
	if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	read(f.phone.AccessToken, 401)
}

func TestModuleNamesExpiryCapacityAndNativeBoundary(t *testing.T) {
	f := signalSetup(t)
	catalog := &moduleNameCatalog{hubs: map[string]moduleNameSnapshot{f.hub.Principal.ID: {names: map[string]string{"module": "旧备注"}, at: time.Now().Add(-3 * time.Minute)}}}
	mux := http.NewServeMux()
	catalog.routes(mux, f.store)
	req := httptest.NewRequest("POST", "/v1/module-names", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+f.phone.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) || strings.Contains(w.Body.String(), "旧备注") {
		t.Fatalf("expired catalogue leaked: %s", w.Body.String())
	}
	for i := 0; i < 1024; i++ {
		catalog.hubs[strings.Repeat("x", i+1)] = moduleNameSnapshot{at: time.Now()}
	}
	data, _ := json.Marshal(map[string]any{"modules": []identity.ModuleChoice{{ID: "module", Alias: "新"}, {ID: "other", Alias: ""}}})
	req = httptest.NewRequest("PUT", "/v1/module-names", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+f.hub.AccessToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("catalogue should be bounded, got %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/v1/module-names", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Authorization", "Bearer "+f.phone.AccessToken)
	w = httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("browser origin accepted on native API")
	}
}
