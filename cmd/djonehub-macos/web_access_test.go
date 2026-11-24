package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/webadmin"
)

func writeWebAccess(t *testing.T, value any) string {
	t.Helper()
	directory := t.TempDir()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "web-access.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestLoadWebAccess(t *testing.T) {
	if access := loadWebAccess(t.TempDir()); access.LANListen != "" || len(access.Hosts) != 0 {
		t.Fatal("missing file must disable everything", access)
	}
	access := loadWebAccess(writeWebAccess(t, webAccess{Version: 1, Hosts: []string{"localhost", "imacmini.sgponte", "bad name", ""}, LANListen: "0.0.0.0:7576"}))
	if access.LANListen != "0.0.0.0:7576" || len(access.Hosts) != 2 {
		t.Fatal("valid entries filtered incorrectly", access)
	}
	access = loadWebAccess(writeWebAccess(t, map[string]any{"version": 2}))
	if access.LANListen != "" || len(access.Hosts) != 0 {
		t.Fatal("unsupported version must reset the file", access)
	}
	access = loadWebAccess(writeWebAccess(t, map[string]any{"version": 1, "lan_listen": "127.0.0.1:7576"}))
	if access.LANListen != "" {
		t.Fatal("loopback lan_listen must be ignored", access)
	}
	access = loadWebAccess(writeWebAccess(t, map[string]any{"version": 1, "lan_listen": "not an address"}))
	if access.LANListen != "" {
		t.Fatal("invalid lan_listen must be ignored", access)
	}
}

func TestLANGateRequiresLoginForAPIButNotShells(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "web-lan")
	auth, err := webadmin.Open(webadmin.Options{Directory: directory, Origin: "http://imacmini.sgponte:7576", Tenant: "local", CookieName: "dj_lan", AllowInsecureHTTP: true, AcceptAnyHost: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/style.css" || r.URL.Path == "/app.js" || strings.HasPrefix(r.URL.Path, "/remote") || strings.HasPrefix(r.URL.Path, "/api/remote-management/session") {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(204)
	})
	h := lanGate(auth, inner)
	call := func(method, path, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://imacmini.sgponte:7576"+path, strings.NewReader(`{}`))
		r.RemoteAddr = "192.0.2.9:1234"
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/", "/style.css", "/app.js", "/remote/", "/remote/app.js"} {
		if w := call("GET", path, ""); w.Code != 200 {
			t.Fatal("shell blocked before login", path, w.Code)
		}
	}
	if w := call("GET", "/api/remote-management/session/status", ""); w.Code != 200 {
		t.Fatal("session status blocked", w.Code)
	}
	if w := call("GET", "/api/devices", ""); w.Code != 401 {
		t.Fatal("management API reachable without login", w.Code)
	}
	if w := call("POST", "/api/sms/send", "http://imacmini.sgponte:7576"); w.Code != 401 {
		t.Fatal("write API reachable without login", w.Code)
	}
	if w := call("GET", "/api/devices", ""); w.Body.String() != "{\"error\":\"management_login_required\"}\n" && w.Code != 401 {
		t.Fatal("unexpected body", w.Body.String())
	}
}
