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

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func TestModuleRoutesAuthenticatedScopedAndUpdated(t *testing.T) {
	f := signalSetup(t)
	client, err := relayclient.New(f.http.URL, relayclient.Options{TLSConfig: f.tls})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	publish := func(token string, routes []callcontrol.ModuleRouteEntry) error {
		return client.PublishModuleRoutes(context.Background(), token, routes)
	}
	read := func(token string, status int) (bool, []callcontrol.ModuleRouteEntry) {
		t.Helper()
		req, _ := http.NewRequest("POST", f.http.URL+"/v1/module-routes", strings.NewReader(`{}`))
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
			Hub       string                        `json:"hub_id"`
			Available bool                          `json:"available"`
			Modules   []callcontrol.ModuleRouteEntry `json:"modules"`
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
	good := []callcontrol.ModuleRouteEntry{
		{ID: "module", EffectiveRoute: "agent", Known: true, AIEnabled: true},
		{ID: "other", EffectiveRoute: "remote", Known: true, RemoteEnabled: true, TargetInstallationID: f.phone.Principal.ID},
	}
	if err := publish(f.hub.AccessToken, good); err != nil {
		t.Fatal(err)
	}
	available, routes := read(f.phone.AccessToken, 200)
	if !available || len(routes) != 1 || routes[0].ID != "module" || routes[0].EffectiveRoute != "agent" || !routes[0].AIEnabled {
		t.Fatalf("phone must only see its authorized line: %+v", routes)
	}
	for _, bad := range [][]callcontrol.ModuleRouteEntry{
		{{ID: "module", EffectiveRoute: "agent", Known: true, AIEnabled: true}},
		{{ID: "module", EffectiveRoute: "agent", Known: true, AIEnabled: true}, {ID: "module", EffectiveRoute: "local", Known: true}},
		{{ID: "module", EffectiveRoute: "agent", Known: true, AIEnabled: true}, {ID: "foreign", EffectiveRoute: "local", Known: true}},
		{{ID: "module", EffectiveRoute: "bogus", Known: true}, {ID: "other", EffectiveRoute: "local", Known: true}},
		{{ID: "module", EffectiveRoute: "agent", Known: true}, {ID: "other", EffectiveRoute: "local", Known: true}},
		{{ID: "module", EffectiveRoute: "remote", Known: true, RemoteEnabled: true}, {ID: "other", EffectiveRoute: "local", Known: true}},
	} {
		if err := publish(f.hub.AccessToken, bad); err == nil {
			t.Fatalf("invalid publication accepted: %+v", bad)
		}
	}
	if err := publish(f.phone.AccessToken, good); err == nil {
		t.Fatal("phone may not publish routes")
	}
	_, routes = read(f.phone.AccessToken, 200)
	if len(routes) != 1 || routes[0].EffectiveRoute != "agent" {
		t.Fatal("invalid publication changed existing routes")
	}
	good[0].EffectiveRoute = "remote"
	good[0].AIEnabled = false
	good[0].RemoteEnabled = true
	good[0].TargetInstallationID = f.phone.Principal.ID
	if err := publish(f.hub.AccessToken, good); err != nil {
		t.Fatal(err)
	}
	_, routes = read(f.phone.AccessToken, 200)
	if routes[0].EffectiveRoute != "remote" || routes[0].TargetInstallationID != f.phone.Principal.ID {
		t.Fatalf("existing phone did not receive route update: %+v", routes[0])
	}
	other := f.enroll(t, "another-tenant")
	if err := publish(other.AccessToken, []callcontrol.ModuleRouteEntry{
		{ID: "module", EffectiveRoute: "agent", Known: true, AIEnabled: true},
		{ID: "other", EffectiveRoute: "local", Known: true},
	}); err != nil {
		t.Fatal(err)
	}
	_, routes = read(f.phone.AccessToken, 200)
	if routes[0].EffectiveRoute != "remote" {
		t.Fatal("cross-hub route leak")
	}
	read("", 401)
	if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	read(f.phone.AccessToken, 401)
}

func TestModuleRoutesExpiryCapacityAndUnknown(t *testing.T) {
	f := signalSetup(t)
	catalog := &moduleRouteCatalog{hubs: map[string]moduleRouteSnapshot{
		f.hub.Principal.ID: {
			routes: map[string]callcontrol.ModuleRouteEntry{"module": {ID: "module", EffectiveRoute: "agent", Known: true, AIEnabled: true}},
			at:     time.Now().Add(-3 * time.Minute),
		},
	}}
	mux := http.NewServeMux()
	catalog.routes(mux, f.store)
	req := httptest.NewRequest("POST", "/v1/module-routes", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+f.phone.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) || strings.Contains(w.Body.String(), `"agent"`) {
		t.Fatalf("expired catalogue leaked: %s", w.Body.String())
	}
	for i := 0; i < 1024; i++ {
		catalog.hubs[strings.Repeat("x", i+1)] = moduleRouteSnapshot{at: time.Now()}
	}
	data, _ := json.Marshal(map[string]any{"modules": []callcontrol.ModuleRouteEntry{
		{ID: "module", EffectiveRoute: "local", Known: true},
		{ID: "other", Known: false},
	}})
	req = httptest.NewRequest("PUT", "/v1/module-routes", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+f.hub.AccessToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("catalogue should be bounded, got %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/v1/module-routes", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Authorization", "Bearer "+f.phone.AccessToken)
	w = httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("browser origin accepted on native API")
	}
}
