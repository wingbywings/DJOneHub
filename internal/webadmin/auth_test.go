package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func authFixture(t *testing.T) (*Auth, http.Handler) {
	t.Helper()
	a, err := Open(Options{Directory: filepath.Join(t.TempDir(), "admin"), Origin: "https://admin.test", Tenant: "home", CookieName: "dj_admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	mux := http.NewServeMux()
	a.Register(mux, "/session")
	mux.Handle("POST /protected", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	return a, a.Guard(mux)
}
func send(h http.Handler, method, path, body string, cookie *http.Cookie, csrf, origin, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://admin.test"+path, strings.NewReader(body))
	r.Host = host
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-DJOneHub-CSRF", csrf)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func setupSession(t *testing.T, a *Auth, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": a.setup.Token, "username": "admin", "password": "a sufficiently long password"})
	w := send(h, "POST", "/session/setup", string(body), nil, "", a.options.Origin, a.host)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	return w.Result().Cookies()[0], result.CSRF
}
func TestSetupLoginCSRFAndExpiration(t *testing.T) {
	a, h := authFixture(t)
	cookie, csrf := setupSession(t, a, h)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal(cookie)
	}
	if _, err := os.Stat(a.SetupPath()); !os.IsNotExist(err) {
		t.Fatal("setup file retained")
	}
	for _, row := range []struct {
		cookie             *http.Cookie
		csrf, origin, host string
		code               int
	}{
		{cookie, csrf, a.options.Origin, a.host, 204}, {cookie, "", a.options.Origin, a.host, 403},
		{cookie, csrf, "https://evil.test", a.host, 403}, {cookie, csrf, a.options.Origin, "evil.test", 403},
		{nil, csrf, a.options.Origin, a.host, 401}, {cookie, csrf, "", a.host, 403},
	} {
		w := send(h, "POST", "/protected", "{}", row.cookie, row.csrf, row.origin, row.host)
		if w.Code != row.code {
			t.Fatal(row, w.Code)
		}
	}
	w := send(h, "POST", "/session/setup", `{}`, nil, "", a.options.Origin, a.host)
	if w.Code != 401 {
		t.Fatal("setup reusable", w.Code)
	}
	data, err := os.ReadFile(filepath.Join(a.options.Directory, "account.json"))
	if err != nil || strings.Contains(string(data), "sufficiently") {
		t.Fatal("plaintext password saved", err)
	}
	a.now = func() time.Time { return time.Now().Add(lifetime + time.Second) }
	w = send(h, "POST", "/protected", "{}", cookie, csrf, a.options.Origin, a.host)
	if w.Code != 401 {
		t.Fatal("expired session", w.Code)
	}
}
func TestRestartRotatesSessionsAndPasswordCanLogin(t *testing.T) {
	a, h := authFixture(t)
	cookie, csrf := setupSession(t, a, h)
	options := a.options
	a.Close()
	b, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	mux := http.NewServeMux()
	b.Register(mux, "/session")
	mux.Handle("POST /protected", b.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	handler := b.Guard(mux)
	w := send(handler, "POST", "/protected", "{}", cookie, csrf, b.options.Origin, b.host)
	if w.Code != 401 {
		t.Fatal("old cookie survived restart")
	}
	w = send(handler, "POST", "/session/login", `{"username":"admin","password":"a sufficiently long password"}`, nil, "", b.options.Origin, b.host)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestUnsafeConfigAndConcurrentOwnerFailClosed(t *testing.T) {
	a, _ := authFixture(t)
	if _, err := Open(a.options); err == nil {
		t.Fatal("two owners")
	}
	a.Close()
	if err := os.Chmod(a.options.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(a.options); err == nil {
		t.Fatal("unsafe directory")
	}
}
func TestLoginRateAndNoSecretInStatus(t *testing.T) {
	a, h := authFixture(t)
	w := send(h, "GET", "/session/status", "", nil, "", "", a.host)
	if strings.Contains(w.Body.String(), a.setup.Token) {
		t.Fatal("setup secret leaked")
	}
	for i := 0; i < 6; i++ {
		w = send(h, "POST", "/session/setup", `{"token":"bad","username":"admin","password":"long-enough-password"}`, nil, "", a.options.Origin, a.host)
	}
	if w.Code != 429 {
		t.Fatal("no rate limit", w.Code)
	}
}
func TestLoopbackHTTPAndRebindingGuard(t *testing.T) {
	a, err := Open(Options{Directory: filepath.Join(t.TempDir(), "admin"), Origin: "http://127.0.0.1:7575", Tenant: "local", CookieName: "dj_local", LoopbackOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	mux := http.NewServeMux()
	a.Register(mux, "/session")
	h := a.Guard(mux)
	r := httptest.NewRequest("GET", "http://127.0.0.1:7575/session/status", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	r.Host = "evil.test:7575"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("rebinding", w.Code)
	}
	r.Host = a.host
	r.RemoteAddr = "192.0.2.1:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("nonlocal", w.Code)
	}
}

func TestHostAliasesExtendLoopbackDeployment(t *testing.T) {
	a, err := Open(Options{Directory: filepath.Join(t.TempDir(), "admin"), Origin: "http://127.0.0.1:7575", Tenant: "local", CookieName: "dj_local", LoopbackOnly: true, LocalTickets: true, LocalNoPassword: true, HostAliases: []string{"localhost", "imacmini.sgponte"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	mux := http.NewServeMux()
	a.Register(mux, "/session")
	h := a.Guard(mux)
	request := func(method, path, host, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:4321"
		r.Host = host
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, host := range []string{"127.0.0.1:7575", "localhost:7575", "imacmini.sgponte:7575"} {
		if w := request("POST", "/session/local", host, "http://"+host); w.Code != 200 {
			t.Fatal("alias bootstrap denied", host, w.Code, w.Body.String())
		}
	}
	if w := request("GET", "/session/status", "localhost:7575", ""); w.Code != 200 {
		t.Fatal("alias GET denied", w.Code)
	}
	if w := request("POST", "/session/local", "evil.test:7575", "http://evil.test:7575"); w.Code != 403 {
		t.Fatal("rebinding host admitted", w.Code)
	}
	if w := request("POST", "/session/local", "127.0.0.1:7575", "http://localhost:7575"); w.Code != 403 {
		t.Fatal("cross-origin alias admitted", w.Code)
	}
	if w := request("POST", "/session/local", "localhost:7575", ""); w.Code != 403 {
		t.Fatal("missing origin write admitted", w.Code)
	}
	for _, aliases := range [][]string{{"name:9999"}, {"bad name"}, {""}, {"under_score.test"}} {
		if _, err := Open(Options{Directory: filepath.Join(t.TempDir(), "admin"), Origin: "http://127.0.0.1:7575", Tenant: "local", CookieName: "dj_local", LoopbackOnly: true, HostAliases: aliases}); err == nil {
			t.Fatal("invalid alias accepted", aliases)
		}
	}
	normalized, err := Open(Options{Directory: filepath.Join(t.TempDir(), "admin"), Origin: "http://127.0.0.1:7575", Tenant: "local", CookieName: "dj_local", LoopbackOnly: true, HostAliases: []string{"LOCALHOST:7575"}})
	if err != nil {
		t.Fatal("normalized alias rejected", err)
	}
	if !normalized.hosts["localhost:7575"] || normalized.hosts["LOCALHOST:7575"] {
		t.Fatal("alias not normalized", normalized.hosts)
	}
	normalized.Close()
}

func TestLANPasswordModeRequiresLoginForAnyHost(t *testing.T) {
	a, err := Open(Options{Directory: filepath.Join(t.TempDir(), "lan"), Origin: "http://imacmini.sgponte:7576", Tenant: "local", CookieName: "dj_lan", AllowInsecureHTTP: true, AcceptAnyHost: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	mux := http.NewServeMux()
	a.Register(mux, "/session")
	mux.Handle("POST /protected", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	h := a.Guard(mux)
	request := func(method, path, host, origin, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.10:5555"
		r.Host = host
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-DJOneHub-CSRF", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/protected", "192.168.1.5:7576", "http://192.168.1.5:7576", `{}`, nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated request admitted", w.Code)
	}
	if w := request("GET", "/session/status", "evil.test:7576", "", ``, nil, ""); w.Code != 200 {
		t.Fatal("status unreachable", w.Code)
	}
	body, _ := json.Marshal(map[string]string{"token": a.setup.Token, "username": "admin", "password": "a sufficiently long password"})
	w := request("POST", "/session/setup", "imacmini.sgponte:7576", "http://imacmini.sgponte:7576", string(body), nil, "")
	if w.Code != 200 {
		t.Fatal("setup failed", w.Code, w.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &session)
	if session.CSRF == "" {
		t.Fatal("setup did not establish a session")
	}
	cookie := w.Result().Cookies()[0]
	if w := request("POST", "/protected", "192.168.1.5:7576", "http://192.168.1.5:7576", `{}`, cookie, session.CSRF); w.Code != 204 {
		t.Fatal("session request denied", w.Code)
	}
	if w := request("POST", "/protected", "192.168.1.5:7576", "http://evil.test", `{}`, cookie, session.CSRF); w.Code != 403 {
		t.Fatal("cross-origin write admitted", w.Code)
	}
	if w := request("POST", "/protected", "192.168.1.5:7576", "http://192.168.1.5:7576", `{}`, cookie, ""); w.Code != 403 {
		t.Fatal("missing CSRF admitted", w.Code)
	}
	if _, err := Open(Options{Directory: t.TempDir(), Origin: "http://imacmini.sgponte:7576", Tenant: "local", CookieName: "dj_lan", LocalNoPassword: true, AllowInsecureHTTP: true}); err == nil {
		t.Fatal("passwordless LAN allowed")
	}
	if _, err := Open(Options{Directory: t.TempDir(), Origin: "http://imacmini.sgponte:7576", Tenant: "local", CookieName: "dj_lan", LocalNoPassword: true, AcceptAnyHost: true}); err == nil {
		t.Fatal("any-host passwordless allowed")
	}
	if _, err := Open(Options{Directory: t.TempDir(), Origin: "http://imacmini.sgponte:7576", Tenant: "local", CookieName: "dj_lan"}); err == nil {
		t.Fatal("plaintext LAN without explicit opt-in allowed")
	}
}
