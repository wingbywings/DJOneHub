//go:build cgo

package relayclient_test

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func fixture(t *testing.T, wrap func(http.Handler) http.Handler) (*relayclient.Client, *identity.Store, identity.Tokens, ed25519.PrivateKey) {
	t.Helper()
	s, err := identity.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	handler := relay.NewServer(s)
	var h http.Handler = handler
	if wrap != nil {
		h = wrap(h)
	}
	server := httptest.NewTLSServer(h)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	api, err := relayclient.New(server.URL, relayclient.Options{TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close(); handler.Close(); server.Close(); s.Close() })
	g, err := s.ProvisionEnrollment(context.Background(), "tenant", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(nil)
	hub, err := api.Enroll(context.Background(), g, key)
	if err != nil {
		t.Fatal(err)
	}
	return api, s, hub, key
}
func TestIdentityClientPairAndSerializedDurableRefresh(t *testing.T) {
	api, s, hub, _ := fixture(t, nil)
	g, err := s.CreatePairing(context.Background(), hub.AccessToken, []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(nil)
	phone, err := api.Pair(context.Background(), g, key)
	if err != nil {
		t.Fatal(err)
	}
	var saves atomic.Int32
	var last identity.Tokens
	credentials, err := relayclient.NewCredentials(api, phone, key, func(next identity.Tokens) error { last = next; saves.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 4)
	failures := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := credentials.RefreshAccess(context.Background())
			if err != nil {
				failures <- err
			} else {
				results <- token
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if saves.Load() != 4 {
		t.Fatal("missing durable saves")
	}
	seen := make(map[string]bool)
	for token := range results {
		if token == "" || seen[token] {
			t.Fatal("unrotated access")
		}
		seen[token] = true
		if _, err := s.Authenticate(context.Background(), token); err != nil {
			t.Fatal("concurrent refresh revoked identity", err)
		}
	}
	if _, err := s.Challenge(context.Background(), last.Principal.ID, last.RefreshToken); err != nil {
		t.Fatal("persisted token is not current", err)
	}
}
func TestFailedCredentialSavePreventsReuse(t *testing.T) {
	api, s, hub, key := fixture(t, nil)
	var saves int
	c, err := relayclient.NewCredentials(api, hub, key, func(identity.Tokens) error { saves++; return errors.New("private storage failed") })
	if err != nil {
		t.Fatal(err)
	}
	if token, err := c.RefreshAccess(context.Background()); token != "" || !errors.Is(err, relayclient.ErrPersistence) {
		t.Fatal("save failure returned usable token", err)
	}
	if _, err := c.RefreshAccess(context.Background()); !errors.Is(err, relayclient.ErrCredentials) {
		t.Fatal("retried spent refresh", err)
	}
	if saves != 1 {
		t.Fatal("retried save without recovery")
	}
	if _, err := s.Authenticate(context.Background(), hub.AccessToken); err != nil {
		t.Fatal("blind retry revoked identity", err)
	}
}
func TestLostRotationResponseDoesNotReplayOldRefresh(t *testing.T) {
	var rotations atomic.Int32
	api, s, hub, key := fixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/auth/refresh" {
				next.ServeHTTP(w, r)
				return
			}
			rotations.Add(1)
			record := httptest.NewRecorder()
			next.ServeHTTP(record, r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(record.Code)
			w.Write([]byte("{")) // server committed; transport lost response
		})
	})
	c, err := relayclient.NewCredentials(api, hub, key, func(identity.Tokens) error { t.Error("saved invalid response"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshAccess(context.Background()); err == nil {
		t.Fatal("truncated response accepted")
	}
	if _, err := c.RefreshAccess(context.Background()); !errors.Is(err, relayclient.ErrCredentials) {
		t.Fatal(err)
	}
	if rotations.Load() != 1 {
		t.Fatal("ambiguous operation replayed")
	}
	if _, err := s.Authenticate(context.Background(), hub.AccessToken); err != nil {
		t.Fatal("retry revoked otherwise valid identity", err)
	}
}
func TestExpiredAccessLoadAndCancelledRefresh(t *testing.T) {
	api, _, hub, key := fixture(t, nil)
	hub.Principal.AccessExpiresAt = time.Now().Add(-time.Hour)
	saves := 0
	c, err := relayclient.NewCredentials(api, hub, key, func(identity.Tokens) error { saves++; return nil })
	if err != nil {
		t.Fatal("expired access blocked refresh recovery", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.RefreshAccess(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.RefreshAccess(context.Background()); err != nil {
		t.Fatal("preflight cancellation burned credential", err)
	}
	if saves != 1 {
		t.Fatal(saves)
	}
}
func TestIdentityClientNeverRedirectsProofAndBoundsResponse(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "oversize", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "oversize":
					w.Write([]byte(strings.Repeat(" ", 32769)))
				case "unknown":
					w.Write([]byte(`{"unexpected":"credential"}`))
				}
			}))
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			api, err := relayclient.New(server.URL, relayclient.Options{TLSConfig: &tls.Config{RootCAs: roots}})
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			_, key, _ := ed25519.GenerateKey(nil)
			if _, err := api.Enroll(context.Background(), identity.Grant{ID: "grant", Secret: strings.Repeat("A", 43)}, key); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("proof sent to redirect target")
	}
	for _, base := range []string{"http://relay.example", "https://user:secret@relay.example", "https://relay.example?token=secret", "https://relay.example/subpath"} {
		if api, err := relayclient.New(base, relayclient.Options{AllowInsecureLoopback: true}); err == nil {
			api.Close()
			t.Fatal("unsafe endpoint", base)
		}
	}
}
