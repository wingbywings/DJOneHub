// Package relay exposes identity APIs and authenticated online call signaling.
package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/turncred"
	"golang.org/x/time/rate"
)

type ipLimit struct {
	limiter *rate.Limiter
	seen    time.Time
}
type limiter struct {
	mu      sync.Mutex
	entries map[string]ipLimit
	global  *rate.Limiter
}

func (l *limiter) allow(address string) bool {
	ip, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.global.AllowN(now, 1) {
		return false
	}
	if len(l.entries) >= 2048 {
		for key, value := range l.entries {
			if now.Sub(value.seen) > 10*time.Minute {
				delete(l.entries, key)
			}
		}
	}
	entry, ok := l.entries[ip]
	if !ok {
		if len(l.entries) >= 2048 {
			return false
		}
		entry = ipLimit{limiter: rate.NewLimiter(4, 20)}
	}
	entry.seen = now
	l.entries[ip] = entry
	return entry.limiter.AllowN(now, 1)
}

type Server struct {
	http.Handler
	signals *signalBroker
}

func (s *Server) Close()                         { s.signals.close() }
func Handler(store *identity.Store) http.Handler { return NewServer(store) }

type Option func(*serverOptions)
type serverOptions struct{ turn *turncred.Issuer }

func WithTURN(issuer *turncred.Issuer) Option { return func(o *serverOptions) { o.turn = issuer } }
func NewServer(store *identity.Store, options ...Option) *Server {
	var config serverOptions
	for _, option := range options {
		option(&config)
	}
	mux := http.NewServeMux()
	signals := newSignalBroker(store)
	mux.HandleFunc("GET /v1/signaling", signals.serve)
	limits := &limiter{entries: make(map[string]ipLimit), global: rate.NewLimiter(50, 100)}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]any{"status": "ok", "version": 1, "capabilities": map[string]bool{"identity": true, "signaling": true, "outgoing_calls": true, "voip_push": false, "turn": config.turn != nil}})
	})
	mux.HandleFunc("POST /v1/ice", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ModuleID string `json:"module_id"`
		}
		if !decode(w, r, &body) {
			return
		}
		token := identity.Bearer(r.Header.Get("Authorization"))
		principal, err := store.Authenticate(r.Context(), token)
		if err == nil {
			_, err = store.AuthorizeModule(r.Context(), token, principal.HubID, body.ModuleID)
		}
		if err != nil {
			result(w, 0, nil, err)
			return
		}
		credentials, err := config.turn.Issue(time.Now())
		result(w, http.StatusOK, credentials, err)
	})
	mux.HandleFunc("POST /v1/hubs/enroll", func(w http.ResponseWriter, r *http.Request) {
		var proof identity.GrantProof
		if !decode(w, r, &proof) {
			return
		}
		tokens, err := store.Enroll(r.Context(), proof)
		result(w, http.StatusCreated, tokens, err)
	})
	mux.HandleFunc("POST /v1/pairings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Modules []string `json:"modules"`
		}
		if !decode(w, r, &body) {
			return
		}
		grant, err := store.CreatePairing(r.Context(), identity.Bearer(r.Header.Get("Authorization")), body.Modules)
		result(w, http.StatusCreated, grant, err)
	})
	mux.HandleFunc("POST /v1/pairings/redeem", func(w http.ResponseWriter, r *http.Request) {
		var proof identity.GrantProof
		if !decode(w, r, &proof) {
			return
		}
		tokens, err := store.Redeem(r.Context(), proof)
		result(w, http.StatusCreated, tokens, err)
	})
	mux.HandleFunc("POST /v1/auth/challenge", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IdentityID   string `json:"identity_id"`
			RefreshToken string `json:"refresh_token"`
		}
		if !decode(w, r, &body) {
			return
		}
		challenge, err := store.Challenge(r.Context(), body.IdentityID, body.RefreshToken)
		result(w, http.StatusOK, challenge, err)
	})
	mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var proof identity.RefreshProof
		if !decode(w, r, &proof) {
			return
		}
		tokens, err := store.Refresh(r.Context(), proof)
		result(w, http.StatusOK, tokens, err)
	})
	mux.HandleFunc("GET /v1/identity", func(w http.ResponseWriter, r *http.Request) {
		principal, err := store.Authenticate(r.Context(), identity.Bearer(r.Header.Get("Authorization")))
		result(w, http.StatusOK, principal, err)
	})
	mux.HandleFunc("GET /v1/installations", func(w http.ResponseWriter, r *http.Request) {
		items, err := store.ListInstallations(r.Context(), identity.Bearer(r.Header.Get("Authorization")))
		result(w, http.StatusOK, items, err)
	})
	mux.HandleFunc("DELETE /v1/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := store.RevokeInstallation(r.Context(), identity.Bearer(r.Header.Get("Authorization")), r.PathValue("id"))
		result(w, http.StatusOK, map[string]bool{"revoked": true}, err)
	})
	return &Server{signals: signals, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("Origin") != "" {
			respond(w, http.StatusForbidden, map[string]string{"error": "native_client_required"})
			return
		}
		// Query strings never carry identity secrets; reject accidental exposure.
		if r.URL.RawQuery != "" {
			respond(w, http.StatusBadRequest, map[string]string{"error": "query_not_supported"})
			return
		}
		if r.URL.Path != "/healthz" && !limits.allow(r.RemoteAddr) {
			w.Header().Set("Retry-After", "1")
			respond(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			return
		}
		mux.ServeHTTP(w, r)
	})}
}
func decode(w http.ResponseWriter, r *http.Request, body any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return false
	}
	return true
}
func result(w http.ResponseWriter, status int, value any, err error) {
	if err == nil {
		respond(w, status, value)
		return
	}
	code := "identity_store_error"
	status = http.StatusInternalServerError
	switch {
	case errors.Is(err, identity.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "invalid_credential"
	case errors.Is(err, identity.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, identity.ErrCapacity):
		status, code = http.StatusTooManyRequests, "capacity_reached"
	case errors.Is(err, identity.ErrRefreshReuse):
		status, code = http.StatusUnauthorized, "refresh_reuse_identity_revoked"
	}
	// Do not expose SQLite errors, raw requests or any credential values.
	respond(w, status, map[string]string{"error": code})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
