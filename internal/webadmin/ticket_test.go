package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLauncherTicketSingleUseExpirySetupAndPlaneIsolation(t *testing.T) {
	a, err := Open(Options{Directory: filepath.Join(t.TempDir(), "local"), Origin: "http://127.0.0.1:12345", Tenant: "local", CookieName: "fixture_local", LoopbackOnly: true, LocalTickets: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	mux := http.NewServeMux()
	a.Register(mux, "/session")
	mux.Handle("POST /protected", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	h := a.Guard(mux)
	call := func(path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", a.options.Origin+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4321"
		r.Header.Set("Origin", a.options.Origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-DJOneHub-CSRF", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	link, err := a.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	ticket := strings.Split(link, "remote-ticket=")[1]
	body, _ := json.Marshal(map[string]string{"ticket": ticket})
	w := call("/session/activate", string(body), nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Secure || !cookie.HttpOnly {
		t.Fatal("loopback cookie")
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &session)
	if w = call("/session/activate", string(body), nil, ""); w.Code != 401 {
		t.Fatal("ticket replay accepted")
	}
	if w = call("/protected", "{}", cookie, session.CSRF); w.Code != 204 {
		t.Fatal("launcher session unusable")
	}
	if w = call("/session/setup", `{"username":"fixture","password":"fixture-test-password"}`, cookie, ""); w.Code != 401 {
		t.Fatal("local setup without CSRF")
	}
	if w = call("/session/setup", `{"username":"fixture","password":"fixture-test-password"}`, cookie, session.CSRF); w.Code != 200 {
		t.Fatal("authorized setup", w.Code)
	}
	expired, _ := a.IssueTicket()
	a.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	body, _ = json.Marshal(map[string]string{"ticket": strings.Split(expired, "remote-ticket=")[1]})
	if w = call("/session/activate", string(body), nil, ""); w.Code != 401 {
		t.Fatal("expired ticket accepted")
	}
	relay, _ := authFixture(t)
	if _, err = relay.IssueTicket(); err == nil {
		t.Fatal("relay issued native local ticket")
	}
	if _, err = Open(Options{Directory: filepath.Join(t.TempDir(), "wrong"), Origin: "https://admin.test", Tenant: "local", CookieName: "fixture", LoopbackOnly: true, LocalTickets: true}); err == nil {
		t.Fatal("public ticket plane accepted")
	}
}
