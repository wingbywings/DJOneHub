//go:build cgo

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/voippush"
)

type testPush struct{}

func (testPush) Environment() string { return "sandbox" }
func (testPush) Send(context.Context, string, callsignal.Message) (voippush.Result, error) {
	panic("test must not send a push")
}

func TestIntegratedAdministrationAndNativeGuards(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "admin")
	store, err := identity.Open(filepath.Join(root, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	native := relay.NewServer(store, relay.WithPush(testPush{}), relay.WithEnrollmentOrigin("https://relay.test"), relay.WithBuild("test"))
	defer native.Close()
	handler, closeAdmin, err := mountAdministration(native, store, dir, "https://relay.test", "home", "/relay-admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeAdmin)
	var cookie *http.Cookie
	csrf := ""
	request := func(method, path, body, origin string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://relay.test"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4567"
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if auth {
			r.AddCookie(cookie)
			r.Header.Set("X-DJOneHub-CSRF", csrf)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/relay-admin/", "/relay-admin/app.js", "/relay-admin/style.css"} {
		w := request("GET", path, "", "", false)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if path == "/relay-admin/" && !strings.Contains(w.Body.String(), `name="djonehub-admin-base" content="/relay-admin/"`) {
			t.Fatal("wrong browser API prefix")
		}
	}
	w := request("GET", "/relay-admin", "", "", false)
	if w.Code != 307 || w.Header().Get("Location") != "/relay-admin/" {
		t.Fatal("slash redirect", w.Code, w.Header())
	}
	if request("GET", "/relay-admin/api/overview", "", "", false).Code != 401 {
		t.Fatal("unprotected overview")
	}
	data, err := os.ReadFile(filepath.Join(dir, "setup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var setup struct{ Token string }
	json.Unmarshal(data, &setup)
	body, _ := json.Marshal(map[string]string{"token": setup.Token, "username": "operator", "password": "test-password-long-enough"})
	w = request("POST", "/relay-admin/api/session/setup", string(body), "https://relay.test", false)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie = w.Result().Cookies()[0]
	var session struct{ CSRF string }
	json.Unmarshal(w.Body.Bytes(), &session)
	csrf = session.CSRF
	w = request("GET", "/relay-admin/api/overview", "", "https://relay.test", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"fresh"`) || !strings.Contains(w.Body.String(), `"environment":"sandbox"`) {
		t.Fatal("direct runtime observation", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "identity", "runtime-status.json")); !os.IsNotExist(err) {
		t.Fatal("test requires no status file")
	}
	for _, path := range []string{"/v1/signaling", "/v1/identity"} {
		if request("GET", path, "", "https://relay.test", true).Code != 403 {
			t.Fatal("native browser Origin guard bypassed", path)
		}
	}
	if request("GET", "/v1/identity", "", "", true).Code != 401 {
		t.Fatal("admin cookie authenticated a native API")
	}
	w = request("GET", "/healthz", "", "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"voip_push":true`) {
		t.Fatal("native health changed", w.Code, w.Body.String())
	}
	if request("POST", "/relay-admin/api/session/logout", "{}", "https://evil.test", true).Code != 403 {
		t.Fatal("foreign Origin allowed")
	}
	r := httptest.NewRequest("GET", "https://relay.test/relay-admin/", nil)
	r.RemoteAddr = "198.51.100.1:1234"
	outside := httptest.NewRecorder()
	handler.ServeHTTP(outside, r)
	if outside.Code != 403 {
		t.Fatal("management listener guard allowed nonloopback request")
	}
	// Browser approval/download/redeem uses the same store as native authentication.
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	enrollment := identity.EnrollmentRequest{Version: 1, ID: "request_" + base64.RawURLEncoding.EncodeToString(pub[:16]), Name: "Test Mac", RelayURL: "https://relay.test", PublicKey: pub, Modules: []identity.ModuleChoice{{ID: "module-a", Alias: "Test"}}, CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	enrollment.Signature = ed25519.Sign(key, identity.EnrollmentRequestMessage(enrollment))
	payload, _ := json.Marshal(enrollment)
	w = request("POST", "/v1/enrollment-requests", string(payload), "", false)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/relay-admin/api/requests/"+enrollment.ID+"/approve", `{"modules":["module-a"]}`, "https://relay.test", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/relay-admin/api/requests/"+enrollment.ID+"/authorization", "{}", "https://relay.test", true)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal(w.Code, w.Body.String())
	}
	var authorization identity.EnrollmentAuthorization
	json.Unmarshal(w.Body.Bytes(), &authorization)
	proof := identity.GrantProof{ID: authorization.Grant.ID, Secret: authorization.Grant.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("enrollment", proof))
	tokens, err := store.Enroll(context.Background(), proof)
	if err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/relay-admin/api/identities", "", "https://relay.test", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), tokens.Principal.ID) || strings.Contains(w.Body.String(), tokens.AccessToken) {
		t.Fatal("shared store / token boundary", w.Code, w.Body.String())
	}
	// Restarting/moving the administration component retains the existing account.
	closeAdmin()
	// The account is portable from the old dedicated host to the new host/path.
	legacy, closeLegacy, err := mountAdministration(native, store, dir, "https://admin.test", "home", "/admin")
	if err != nil {
		t.Fatal(err)
	}
	oldLogin := httptest.NewRequest("POST", "https://admin.test/admin/api/session/login", strings.NewReader(`{"username":"operator","password":"test-password-long-enough"}`))
	oldLogin.RemoteAddr = "127.0.0.1:4567"
	oldLogin.Header.Set("Origin", "https://admin.test")
	oldLogin.Header.Set("Content-Type", "application/json")
	oldResponse := httptest.NewRecorder()
	legacy.ServeHTTP(oldResponse, oldLogin)
	if oldResponse.Code != 200 {
		t.Fatal("account could not change host", oldResponse.Code, oldResponse.Body.String())
	}
	closeLegacy()
	handler, closeAdmin, err = mountAdministration(native, store, dir, "https://relay.test", "home", "/relay-admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeAdmin)
	if request("GET", "/relay-admin/api/overview", "", "https://relay.test", true).Code != 401 {
		t.Fatal("old memory session survived restart")
	}
	w = request("POST", "/relay-admin/api/session/login", `{"username":"operator","password":"test-password-long-enough"}`, "https://relay.test", false)
	if w.Code != 200 {
		t.Fatal("account did not survive restart", w.Code, w.Body.String())
	}
}

func TestIntegratedAdminArgumentsFailBeforeOpeningState(t *testing.T) {
	for _, extra := range [][]string{{"--admin-tenant", "home"}, {"--admin-state-dir", "relative", "--admin-tenant", "home"}, {"--admin-state-dir", "/tmp/admin", "--admin-tenant", "home"}} {
		state := filepath.Join(t.TempDir(), "identity")
		args := append([]string{"serve", "--state-dir", state, "--listen", "127.0.0.1:18443", "--insecure-loopback"}, extra...)
		if run(args) == nil {
			t.Fatal("invalid management arguments accepted")
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Fatal("invalid config modified state")
		}
	}
}
