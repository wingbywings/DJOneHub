//go:build cgo

package relayadmin

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relaystatus"
	"github.com/iniwex5/vohive/internal/webadmin"
)

type fixture struct {
	store   *identity.Store
	server  *Server
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func adminFixture(t *testing.T) *fixture {
	return adminFixtureRuntime(t, nil)
}
func adminFixtureRuntime(t *testing.T, observe func() relaystatus.Observation) *fixture {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "admin")
	auth, err := webadmin.Open(webadmin.Options{Directory: directory, Origin: "https://admin.test", Tenant: "home", CookieName: "dj_admin"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Auth: auth, Identity: store, Directory: directory, RelayOrigin: "https://relay.test", Build: "test", RuntimeStatus: observe})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, server: s, handler: s}
	t.Cleanup(func() { s.Close(); auth.Close(); store.Close() })
	data, err := os.ReadFile(auth.SetupPath())
	if err != nil {
		t.Fatal(err)
	}
	var setup struct {
		Token string `json:"token"`
	}
	json.Unmarshal(data, &setup)
	body, _ := json.Marshal(map[string]string{"token": setup.Token, "username": "operator", "password": "a sufficiently long password"})
	w := f.request("POST", "/admin/api/session/setup", body, false)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.cookie = w.Result().Cookies()[0]
	var session struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &session)
	f.csrf = session.CSRF
	return f
}
func (f *fixture) request(method, path string, body []byte, authenticated bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://admin.test"+path, strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://admin.test")
	if authenticated {
		r.AddCookie(f.cookie)
		r.Header.Set("X-DJOneHub-CSRF", f.csrf)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func submitRequest(t *testing.T, f *fixture) (identity.EnrollmentRequest, ed25519.PrivateKey) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	r := identity.EnrollmentRequest{Version: 1, ID: "request_" + base64.RawURLEncoding.EncodeToString(pub[:16]), Name: "My Mac", RelayURL: "https://relay.test", PublicKey: pub, Modules: []identity.ModuleChoice{{ID: "module-a", Alias: "Xiaomi"}, {ID: "module-b", Alias: "Xiaomi"}}, CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	r.Signature = ed25519.Sign(key, identity.EnrollmentRequestMessage(r))
	data, _ := json.Marshal(r)
	w := f.request("POST", "/admin/api/requests/import", data, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return r, key
}
func TestBrowserApprovalDownloadRegistrationAndManagement(t *testing.T) {
	f := adminFixture(t)
	r, key := submitRequest(t, f)
	w := f.request("POST", "/admin/api/requests/"+r.ID+"/approve", []byte(`{"modules":["module-a"]}`), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.request("POST", "/admin/api/requests/"+r.ID+"/authorization", []byte(`{}`), true)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	var file identity.EnrollmentAuthorization
	if json.Unmarshal(w.Body.Bytes(), &file) != nil {
		t.Fatal(w.Body.String())
	}
	proof := identity.GrantProof{ID: file.Grant.ID, Secret: file.Grant.Secret, PublicKey: key.Public().(ed25519.PublicKey)}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("enrollment", proof))
	tokens, err := f.store.Enroll(context.Background(), proof)
	if err != nil {
		t.Fatal(err)
	}
	w = f.request("GET", "/admin/api/identities", nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), tokens.Principal.ID) || strings.Contains(w.Body.String(), tokens.AccessToken) {
		t.Fatal(w.Body.String())
	}
	w = f.request("GET", "/admin/api/audit", nil, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), file.Grant.Secret) || !strings.Contains(w.Body.String(), "issue_authorization") {
		t.Fatal(w.Body.String())
	}
	w = f.request("POST", "/admin/api/requests/"+r.ID+"/authorization", []byte(`{}`), true)
	if w.Code != 400 {
		t.Fatal("consumed authorization downloaded again", w.Code)
	}
	w = f.request("POST", "/admin/api/identities/"+tokens.Principal.ID+"/revoke", []byte(`{}`), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = f.store.Authenticate(context.Background(), tokens.AccessToken); err == nil {
		t.Fatal("revoked access valid")
	}
	w = f.request("POST", "/admin/api/identities/"+tokens.Principal.ID+"/delete", []byte(`{}`), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	requests, err := f.store.AdminEnrollmentRequests(context.Background(), "home")
	if err != nil || len(requests) != 1 || requests[0].State != "completed" {
		t.Fatal("completed enrollment history lost", requests, err)
	}
	w = f.request("POST", "/admin/api/requests/"+r.ID+"/authorization", []byte(`{}`), true)
	if w.Code != 400 {
		t.Fatal("deletion reopened a consumed authorization", w.Code)
	}
}
func TestManagementAuthorizationAndNativeOriginStaySeparate(t *testing.T) {
	f := adminFixture(t)
	r, _ := submitRequest(t, f)
	w := f.request("GET", "/admin/api/requests", nil, false)
	if w.Code != 401 {
		t.Fatal("unprotected list", w.Code)
	}
	request := httptest.NewRequest("POST", "https://admin.test/admin/api/requests/"+r.ID+"/approve", strings.NewReader(`{"modules":["module-a"]}`))
	request.AddCookie(f.cookie)
	request.Header.Set("Origin", "https://evil.test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-DJOneHub-CSRF", f.csrf)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	native := relay.NewServer(f.store, relay.WithEnrollmentOrigin("https://relay.test"))
	defer native.Close()
	request = httptest.NewRequest("POST", "https://relay.test/v1/enrollment-requests", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://admin.test")
	w = httptest.NewRecorder()
	native.ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal("browser gained native access", w.Code)
	}
}

func TestDeleteRevokedIdentityRequiresScopeSessionAndAudit(t *testing.T) {
	f := adminFixture(t)
	ctx := context.Background()
	g, err := f.store.ProvisionEnrollment(ctx, "home", []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	pub, key, _ := ed25519.GenerateKey(nil)
	proof := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("enrollment", proof))
	tokens, err := f.store.Enroll(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/api/identities/" + tokens.Principal.ID + "/delete"
	if w := f.request("POST", path, []byte(`{}`), false); w.Code != 401 {
		t.Fatal("delete without session", w.Code)
	}
	if w := f.request("POST", path, []byte(`{}`), true); w.Code != 400 {
		t.Fatal("delete active identity", w.Code, w.Body.String())
	}
	if err = f.store.AdminRevokeTenant(ctx, "home", tokens.Principal.ID); err != nil {
		t.Fatal(err)
	}
	saved := f.csrf
	f.csrf = "wrong"
	if w := f.request("POST", path, []byte(`{}`), true); w.Code != 403 {
		t.Fatal("delete without CSRF", w.Code)
	}
	f.csrf = saved
	w := f.request("POST", path, []byte(`{}`), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted_count":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.request("GET", "/admin/api/identities", nil, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), tokens.Principal.ID) {
		t.Fatal("deleted identity still listed", w.Code, w.Body.String())
	}
	w = f.request("GET", "/admin/api/audit", nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "delete_revoked_identity") || !strings.Contains(w.Body.String(), tokens.Principal.ID) {
		t.Fatal("deletion audit missing", w.Code, w.Body.String())
	}
}

func TestUnavailableAuditPreventsIdentityDeletion(t *testing.T) {
	f := adminFixture(t)
	ctx := context.Background()
	g, err := f.store.ProvisionEnrollment(ctx, "home", []string{"module-a"})
	if err != nil {
		t.Fatal(err)
	}
	pub, key, _ := ed25519.GenerateKey(nil)
	proof := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("enrollment", proof))
	tokens, err := f.store.Enroll(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.AdminRevokeTenant(ctx, "home", tokens.Principal.ID); err != nil {
		t.Fatal(err)
	}
	f.server.audit.db.Close()
	w := f.request("POST", "/admin/api/identities/"+tokens.Principal.ID+"/delete", []byte(`{}`), true)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	items, err := f.store.AdminIdentities(ctx, "home")
	if err != nil || len(items) != 1 {
		t.Fatal("delete proceeded without audit", items, err)
	}
}
func TestFailedAuditDoesNotApprove(t *testing.T) {
	f := adminFixture(t)
	r, _ := submitRequest(t, f)
	f.server.audit.db.Close()
	w := f.request("POST", "/admin/api/requests/"+r.ID+"/approve", []byte(`{"modules":["module-a"]}`), true)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	items, err := f.store.AdminEnrollmentRequests(context.Background(), "home")
	if err != nil || items[0].State != "pending" {
		t.Fatal(items, err)
	}
}
func TestAdminAssetsUseLocalResourcesAndNoSecrets(t *testing.T) {
	f := adminFixture(t)
	for _, path := range []string{"/admin/", "/admin/app.js", "/admin/style.css"} {
		w := f.request("GET", path, nil, false)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal(path, w.Code)
		}
		if strings.Contains(w.Body.String(), f.cookie.Value) {
			t.Fatal("session leaked into assets")
		}
	}
}

// Opt-in preview uses only temporary SQLite and test credentials. It never
// connects to an external Relay or reads a real Keychain/APNs configuration.
func TestAdminBrowserPreview(t *testing.T) {
	if os.Getenv("DJONEHUB_ADMIN_BROWSER_PREVIEW") != "1" {
		t.Skip("opt-in local browser fixture")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	directory := filepath.Join(t.TempDir(), "admin")
	auth, err := webadmin.Open(webadmin.Options{Directory: directory, Origin: origin, Tenant: "home", CookieName: "dj_preview", LoopbackOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	server, err := New(Options{Auth: auth, Identity: store, Directory: directory, RelayOrigin: "https://relay.test", Build: "browser-preview"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	data, _ := os.ReadFile(auth.SetupPath())
	var setup struct {
		Token string `json:"token"`
	}
	json.Unmarshal(data, &setup)
	body, _ := json.Marshal(map[string]string{"token": setup.Token, "username": "preview", "password": "preview-test-password"})
	request := httptest.NewRequest("POST", origin+"/admin/api/session/setup", strings.NewReader(string(body)))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Origin", origin)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	f := &fixture{store: store}
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	r := identity.EnrollmentRequest{Version: 1, ID: "request_" + base64.RawURLEncoding.EncodeToString(pub[:16]), Name: "家庭 Mac", RelayURL: "https://relay.test", PublicKey: pub, Modules: []identity.ModuleChoice{{ID: "dji-module-a", Alias: "Xiaomi"}, {ID: "dji-module-b", Alias: "Xiaomi"}}, CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	r.Signature = ed25519.Sign(key, identity.EnrollmentRequestMessage(r))
	if _, err = f.store.SubmitEnrollmentRequest(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 3 * time.Second}
	go httpServer.Serve(listener)
	defer httpServer.Close()
	t.Log("PREVIEW_URL=" + origin + "/admin/")
	select {
	case <-time.After(180 * time.Second):
	case <-t.Context().Done():
	}
}
