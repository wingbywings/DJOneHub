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

func TestLocalPasswordlessSessionStillRequiresLoopbackOriginAndCSRF(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// An obsolete local account is not read or changed in passwordless mode.
	account := filepath.Join(dir, "account.json")
	if err := os.WriteFile(account, []byte("obsolete account fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Options{Directory: dir, Origin: "http://127.0.0.1:12345", Tenant: "local", CookieName: "fixture_local", LoopbackOnly: true, LocalTickets: true, LocalNoPassword: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	mux := http.NewServeMux()
	a.Register(mux, "/session")
	mux.Handle("POST /protected", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	h := a.Guard(mux)
	call := func(path, peer, origin, host string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", a.options.Origin+path, strings.NewReader(`{}`))
		r.RemoteAddr = peer
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
	for _, values := range [][3]string{{"192.0.2.10:1234", a.options.Origin, a.host}, {"127.0.0.1:4321", "https://evil.test", a.host}, {"127.0.0.1:4321", "", a.host}, {"127.0.0.1:4321", a.options.Origin, "evil.test"}} {
		if w := call("/session/local", values[0], values[1], values[2], nil, ""); w.Code != 403 {
			t.Fatal("invalid bootstrap admitted", values, w.Code)
		}
	}
	w := call("/session/local", "127.0.0.1:4321", a.options.Origin, a.host, nil, "")
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	var session struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal(w.Body.Bytes(), &session) != nil || session.CSRF == "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("invalid local session")
	}
	if w = call("/protected", "127.0.0.1:4321", a.options.Origin, a.host, cookie, ""); w.Code != 403 {
		t.Fatal("write without CSRF", w.Code)
	}
	if w = call("/protected", "127.0.0.1:4321", a.options.Origin, a.host, cookie, session.CSRF); w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = call("/session/local", "127.0.0.1:4321", a.options.Origin, a.host, cookie, "")
	var reused struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &reused)
	if w.Code != 200 || reused.CSRF != session.CSRF || len(w.Result().Cookies()) != 0 {
		t.Fatal("second tab rotated browser session")
	}
	a.now = func() time.Time { return time.Now().Add(lifetime + time.Minute) }
	w = call("/session/local", "127.0.0.1:4321", a.options.Origin, a.host, cookie, "")
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].Value == cookie.Value {
		t.Fatal("expired session not renewed")
	}
	for _, path := range []string{"/session/setup", "/session/login"} {
		if w = call(path, "127.0.0.1:4321", a.options.Origin, a.host, nil, ""); w.Code != 404 {
			t.Fatal("password route retained", path, w.Code)
		}
	}
	data, err := os.ReadFile(account)
	if err != nil || string(data) != "obsolete account fixture" {
		t.Fatal("old account changed", err)
	}
	if _, err = os.Stat(a.SetupPath()); !os.IsNotExist(err) {
		t.Fatal("setup file generated", err)
	}
}

func TestPasswordlessModeCannotEnablePublicRelayAccess(t *testing.T) {
	for _, origin := range []string{"https://admin.test", "https://127.0.0.1:12345", "http://admin.test"} {
		if _, err := Open(Options{Directory: t.TempDir(), Origin: origin, Tenant: "home", CookieName: "fixture", LoopbackOnly: true, LocalNoPassword: true}); err == nil {
			t.Fatal("public/passwordless admin allowed", origin)
		}
	}
	if _, err := Open(Options{Directory: t.TempDir(), Origin: "http://127.0.0.1:12345", Tenant: "local", CookieName: "fixture", LocalNoPassword: true}); err == nil {
		t.Fatal("passwordless without loopback guard allowed")
	}
	a, handler := authFixture(t)
	w := send(handler, "POST", "/session/local", `{}`, nil, "", a.options.Origin, a.host)
	if w.Code != 404 {
		t.Fatal("Relay exposes passwordless bootstrap", w.Code)
	}
}
