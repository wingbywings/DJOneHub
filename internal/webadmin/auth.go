// Package webadmin authenticates browser management independently of native
// Relay/AudioHost credentials. It never grants access to a native bearer token.
package webadmin

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iniwex5/vohive/internal/relayprofile"
)

var ErrConfig = errors.New("invalid or unavailable private Web administration configuration")

const iterations = 600000
const lifetime = 30 * time.Minute

type Options struct {
	Directory    string
	Origin       string
	Tenant       string
	CookieName   string
	LoopbackOnly bool
	LocalTickets bool // launcher-owned native control may issue short-lived tickets
}
type passwordRecord struct {
	Version    int    `json:"version"`
	Username   string `json:"username"`
	Tenant     string `json:"tenant"`
	Salt       []byte `json:"salt"`
	Hash       []byte `json:"hash"`
	Iterations int    `json:"iterations"`
}
type setupRecord struct {
	Version   int       `json:"version"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}
type session struct {
	csrf    string
	expires time.Time
}
type Auth struct {
	mu           sync.Mutex
	options      Options
	host         string
	secure       bool
	account      *passwordRecord
	setup        setupRecord
	sessions     map[[32]byte]session
	tickets      map[[32]byte]time.Time
	now          func() time.Time
	attempts     int
	attemptStart time.Time
	unlock       func()
	closed       bool
}

func token() string { return base64.RawURLEncoding.EncodeToString(random(32)) }
func random(n int) []byte {
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		panic("system random source unavailable")
	}
	return data
}
func validID(s string) bool {
	return s != "" && len(s) <= 64 && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) < 0
}
func Open(options Options) (*Auth, error) {
	origin, err := url.Parse(options.Origin)
	if err != nil || origin.User != nil || origin.Host == "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" || !filepath.IsAbs(options.Directory) || !validID(options.Tenant) || !validID(options.CookieName) {
		return nil, ErrConfig
	}
	secure := origin.Scheme == "https"
	if options.LocalTickets && (!options.LoopbackOnly || origin.Scheme != "http" || net.ParseIP(origin.Hostname()) == nil || !net.ParseIP(origin.Hostname()).IsLoopback()) {
		return nil, ErrConfig
	}
	if !secure && (origin.Scheme != "http" || net.ParseIP(origin.Hostname()) == nil || !net.ParseIP(origin.Hostname()).IsLoopback()) {
		return nil, ErrConfig
	}
	if err = os.MkdirAll(options.Directory, 0700); err != nil {
		return nil, err
	}
	unlock, err := lockDirectory(options.Directory)
	if err != nil {
		return nil, err
	}
	a := &Auth{options: options, host: origin.Host, secure: secure, sessions: make(map[[32]byte]session), tickets: make(map[[32]byte]time.Time), now: time.Now, unlock: unlock}
	success := false
	defer func() {
		if !success {
			unlock()
		}
	}()
	var record passwordRecord
	err = relayprofile.ReadPrivateJSON(filepath.Join(options.Directory, "account.json"), &record)
	if err == nil {
		if record.Version != 1 || record.Tenant != options.Tenant || !validID(record.Username) || len(record.Salt) != 32 || len(record.Hash) != 32 || record.Iterations != iterations {
			return nil, ErrConfig
		}
		a.account = &record
		// A setup file left behind by a crash after account commit is obsolete.
		_ = os.Remove(a.SetupPath())
	} else {
		if _, statErr := os.Lstat(filepath.Join(options.Directory, "account.json")); !errors.Is(statErr, os.ErrNotExist) {
			return nil, ErrConfig
		}
		if err = relayprofile.ReadPrivateJSON(a.SetupPath(), &a.setup); err != nil {
			if _, statErr := os.Lstat(a.SetupPath()); !errors.Is(statErr, os.ErrNotExist) {
				return nil, ErrConfig
			}
			a.setup = setupRecord{1, token(), a.now().Add(lifetime).UTC()}
			if err = relayprofile.WritePrivateJSON(a.SetupPath(), a.setup); err != nil {
				return nil, err
			}
		} else if a.setup.Version != 1 || len(a.setup.Token) != 43 {
			return nil, ErrConfig
		}
		// Expired setup is explicitly rotated only on restart, never by an HTTP GET.
		if !a.setup.ExpiresAt.After(a.now()) {
			if err = os.Remove(a.SetupPath()); err != nil {
				return nil, err
			}
			a.setup = setupRecord{1, token(), a.now().Add(lifetime).UTC()}
			if err = relayprofile.WritePrivateJSON(a.SetupPath(), a.setup); err != nil {
				return nil, err
			}
		}
	}
	success = true
	return a, nil
}
func (a *Auth) SetupPath() string { return filepath.Join(a.options.Directory, "setup.json") }
func (a *Auth) Tenant() string    { return a.options.Tenant }
func (a *Auth) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		clear(a.sessions)
		clear(a.tickets)
		a.unlock()
	}
}
func (a *Auth) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/status", a.status)
	mux.HandleFunc("POST "+prefix+"/setup", a.setupAccount)
	mux.HandleFunc("POST "+prefix+"/login", a.login)
	mux.Handle("POST "+prefix+"/logout", a.Require(http.HandlerFunc(a.logout)))
	if a.options.LocalTickets {
		mux.HandleFunc("POST "+prefix+"/activate", a.activate)
	}
}

// Guard applies to both assets and APIs. Caller supplies the exact public
// origin; proxy-forwarded headers never override it.
func (a *Auth) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if r.Host != a.host || a.options.LoopbackOnly && (err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
			respond(w, http.StatusForbidden, map[string]string{"error": "invalid_management_host"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != a.options.Origin {
			respond(w, http.StatusForbidden, map[string]string{"error": "management_origin_required"})
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && (r.Header.Get("Origin") != a.options.Origin || !jsonContentType(r.Header.Get("Content-Type"))) {
			respond(w, http.StatusForbidden, map[string]string{"error": "same_origin_json_required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		s, ok := a.lookup(r)
		a.mu.Unlock()
		if !ok {
			respond(w, http.StatusUnauthorized, map[string]string{"error": "management_login_required"})
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-DJOneHub-CSRF")), []byte(s.csrf)) != 1 {
			respond(w, http.StatusForbidden, map[string]string{"error": "csrf_required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Caller holds mu. Cookie tokens are hashed even in the ephemeral map.
func (a *Auth) lookup(r *http.Request) (session, bool) {
	if a.closed {
		return session{}, false
	}
	cookie, err := r.Cookie(a.options.CookieName)
	if err != nil || len(cookie.Value) != 43 {
		return session{}, false
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	s, ok := a.sessions[hash]
	if ok && !s.expires.After(a.now()) {
		delete(a.sessions, hash)
		ok = false
	}
	return s, ok
}
func (a *Auth) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.lookup(r)
	body := map[string]any{"configured": a.account != nil, "authenticated": ok}
	if ok {
		body["csrf"] = s.csrf
		body["expires_at"] = s.expires
		body["tenant"] = a.options.Tenant
		if a.account != nil {
			body["username"] = a.account.Username
		}
	}
	respond(w, http.StatusOK, body)
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid_management_request"})
		return false
	}
	return true
}
func (a *Auth) attempt() bool {
	now := a.now()
	if now.Sub(a.attemptStart) >= time.Minute {
		a.attempts = 0
		a.attemptStart = now
	}
	a.attempts++
	return a.attempts <= 5
}
func validPassword(s string) bool { return utf8.ValidString(s) && len(s) >= 12 && len(s) <= 256 }
func (a *Auth) setupAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.attempt() {
		respond(w, http.StatusTooManyRequests, map[string]string{"error": "login_rate_limited"})
		return
	}
	s, local := a.lookup(r)
	local = local && a.options.LocalTickets && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-DJOneHub-CSRF")), []byte(s.csrf)) == 1
	if a.account != nil || !local && (!a.setup.ExpiresAt.After(a.now()) || subtle.ConstantTimeCompare([]byte(body.Token), []byte(a.setup.Token)) != 1) {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "setup_denied"})
		return
	}
	if !validID(body.Username) || !validPassword(body.Password) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "username_or_password_invalid"})
		return
	}
	salt := random(32)
	hash, err := pbkdf2.Key(sha256.New, body.Password, salt, iterations, 32)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "setup_failed"})
		return
	}
	record := passwordRecord{1, body.Username, a.options.Tenant, salt, hash, iterations}
	if relayprofile.WritePrivateJSON(filepath.Join(a.options.Directory, "account.json"), record) != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "setup_failed"})
		return
	}
	a.account = &record
	a.setup = setupRecord{}
	_ = os.Remove(a.SetupPath())
	a.establish(w)
}
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.attempt() {
		respond(w, http.StatusTooManyRequests, map[string]string{"error": "login_rate_limited"})
		return
	}
	if a.account == nil || !validID(body.Username) || !validPassword(body.Password) {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "login_denied"})
		return
	}
	hash, err := pbkdf2.Key(sha256.New, body.Password, a.account.Salt, a.account.Iterations, 32)
	if err != nil || subtle.ConstantTimeCompare(hash, a.account.Hash) != 1 || subtle.ConstantTimeCompare([]byte(body.Username), []byte(a.account.Username)) != 1 {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "login_denied"})
		return
	}
	// Reauthentication rotates this browser's prior session.
	if cookie, err := r.Cookie(a.options.CookieName); err == nil {
		delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
	}
	a.establish(w)
}
func (a *Auth) establish(w http.ResponseWriter) {
	for key, s := range a.sessions {
		if !s.expires.After(a.now()) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 64 {
		respond(w, http.StatusTooManyRequests, map[string]string{"error": "session_capacity"})
		return
	}
	t := token()
	s := session{token(), a.now().Add(lifetime).UTC()}
	a.sessions[sha256.Sum256([]byte(t))] = s
	http.SetCookie(w, &http.Cookie{Name: a.options.CookieName, Value: t, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(lifetime.Seconds())})
	respond(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": s.csrf, "expires_at": s.expires})
}
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cookie, err := r.Cookie(a.options.CookieName); err == nil {
		delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
	}
	http.SetCookie(w, &http.Cookie{Name: a.options.CookieName, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respond(w, http.StatusOK, map[string]bool{"authenticated": false})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *Auth) Username() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account == nil {
		return ""
	}
	return a.account.Username
}

func jsonContentType(value string) bool {
	typ, _, err := mime.ParseMediaType(value)
	return err == nil && typ == "application/json"
}

// IssueTicket is deliberately not a browser route. Only the authenticated native
// launcher calls it. Tokens are never shared with Relay/AudioHost credentials.
func (a *Auth) IssueTicket() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.options.LocalTickets {
		return "", ErrConfig
	}
	for key, expiry := range a.tickets {
		if !expiry.After(a.now()) {
			delete(a.tickets, key)
		}
	}
	if len(a.tickets) >= 16 {
		return "", ErrConfig
	}
	t := token()
	a.tickets[sha256.Sum256([]byte(t))] = a.now().Add(2 * time.Minute)
	return a.options.Origin + "/remote/#remote-ticket=" + t, nil
}
func (a *Auth) activate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ticket string `json:"ticket"`
	}
	if !decode(w, r, &body) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := sha256.Sum256([]byte(body.Ticket))
	expiry, ok := a.tickets[key]
	if a.closed || len(body.Ticket) != 43 || !ok || !expiry.After(a.now()) {
		respond(w, 401, map[string]string{"error": "activation_denied"})
		return
	}
	delete(a.tickets, key)
	if cookie, err := r.Cookie(a.options.CookieName); err == nil {
		delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
	}
	a.establish(w)
}
