package main

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/iniwex5/vohive/internal/webadmin"
)

// webAccess mirrors ~/Library/Application Support/DJOneHub/web-access.json:
// optional extra Host aliases for the loopback listener (for example names
// mapped to 127.0.0.1 in /etc/hosts) and an opt-in second listener that
// exposes the full management UI to other devices with password login. The
// file is absent by default and anything invalid is ignored with a log line,
// so a typo can never widen or remove protection silently.
type webAccess struct {
	Version   int      `json:"version"`
	Hosts     []string `json:"hosts"`
	LANListen string   `json:"lan_listen"`
}

func loadWebAccess(directory string) webAccess {
	access := webAccess{}
	data, err := os.ReadFile(filepath.Join(directory, "web-access.json"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("web access configuration ignored: %v", err)
		}
		return access
	}
	if json.Unmarshal(data, &access) != nil || access.Version != 1 {
		log.Print("web access configuration ignored: invalid JSON or unsupported version")
		return webAccess{}
	}
	hosts := make([]string, 0, len(access.Hosts))
	for _, host := range access.Hosts {
		if webAccessHost(host) {
			hosts = append(hosts, host)
		} else {
			log.Printf("web access host alias ignored: %q", host)
		}
	}
	access.Hosts = hosts
	if access.LANListen != "" && !webAccessListen(access.LANListen) {
		log.Printf("web access lan_listen ignored: %q", access.LANListen)
		access.LANListen = ""
	}
	return access
}

func webAccessHost(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':')
	}) < 0
}

func webAccessListen(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" || host == "" {
		return false
	}
	// The passwordless loopback listener already covers loopback addresses.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// lanWebAdmin opens the password-protected management auth for the LAN
// listener. First use follows the standard setup-token flow; the token lives
// in the private web-lan state directory until an account is created.
func lanWebAdmin(directory, listen string, hosts []string) (*webadmin.Auth, error) {
	_, port, _ := net.SplitHostPort(listen)
	host := "127.0.0.1"
	if len(hosts) > 0 && hosts[0] != "" {
		host = hosts[0]
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
	}
	return webadmin.Open(webadmin.Options{
		Directory: filepath.Join(directory, "web-lan"), Origin: "http://" + net.JoinHostPort(host, port),
		Tenant: "local", CookieName: "djonehub_lan_management",
		AllowInsecureHTTP: true, AcceptAnyHost: true,
	})
}

// lanGate wraps the complete management surface for the LAN listener. Page
// shells and the session endpoints stay reachable before login so a browser
// can render the login form; every other route requires an authenticated
// session with CSRF for writes.
func lanGate(auth *webadmin.Auth, next http.Handler) http.Handler {
	shell := func(r *http.Request) bool {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return false
		}
		path := r.URL.Path
		return path == "/" || path == "/index.html" || path == "/style.css" || path == "/app.js" ||
			path == "/remote" || path == "/remote/" || strings.HasPrefix(path, "/remote/")
	}
	return auth.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shell(r) || strings.HasPrefix(r.URL.Path, remoteWebAPI+"/session") {
			next.ServeHTTP(w, r)
			return
		}
		auth.Require(next).ServeHTTP(w, r)
	}))
}
