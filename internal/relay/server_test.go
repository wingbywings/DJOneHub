//go:build cgo

package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

func request(h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.RemoteAddr = "192.0.2.1:12000"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func readResponse[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var result T
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func apiStore(t *testing.T) (*identity.Store, http.Handler) {
	t.Helper()
	s, err := identity.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, Handler(s)
}
func signed(kind string, g identity.Grant) (identity.GrantProof, ed25519.PrivateKey) {
	pub, key, _ := ed25519.GenerateKey(nil)
	p := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
	p.Signature = ed25519.Sign(key, identity.GrantMessage(kind, p))
	return p, key
}
func TestIdentityHTTPEnrollmentPairingRefreshAndRevoke(t *testing.T) {
	s, h := apiStore(t)
	grant, err := s.ProvisionEnrollment(context.Background(), "tenant", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := signed("enrollment", grant)
	hub := readResponse[identity.Tokens](t, request(h, "POST", "/v1/hubs/enroll", "", proof), 201)
	pair := readResponse[identity.Grant](t, request(h, "POST", "/v1/pairings", hub.AccessToken, map[string]any{"modules": []string{"module"}}), 201)
	proof, key := signed("pairing", pair)
	phone := readResponse[identity.Tokens](t, request(h, "POST", "/v1/pairings/redeem", "", proof), 201)
	list := readResponse[[]identity.Installation](t, request(h, "GET", "/v1/installations", hub.AccessToken, nil), 200)
	if len(list) != 1 || list[0].ID != phone.Principal.ID {
		t.Fatal(list)
	}
	c := readResponse[identity.Challenge](t, request(h, "POST", "/v1/auth/challenge", "", map[string]string{"identity_id": phone.Principal.ID, "refresh_token": phone.RefreshToken}), 200)
	rp := identity.RefreshProof{IdentityID: phone.Principal.ID, RefreshToken: phone.RefreshToken, ChallengeID: c.ID, Challenge: c.Value}
	rp.Signature = ed25519.Sign(key, identity.RefreshMessage(rp))
	rotated := readResponse[identity.Tokens](t, request(h, "POST", "/v1/auth/refresh", "", rp), 200)
	w := request(h, "GET", "/v1/identity", rotated.AccessToken, nil)
	readResponse[identity.Principal](t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential response cacheable")
	}
	readResponse[map[string]bool](t, request(h, "DELETE", "/v1/installations/"+phone.Principal.ID, hub.AccessToken, nil), 200)
	if w := request(h, "GET", "/v1/identity", rotated.AccessToken, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	list = readResponse[[]identity.Installation](t, request(h, "GET", "/v1/installations", hub.AccessToken, nil), 200)
	if len(list) != 0 {
		t.Fatal("revoked device still offered as target")
	}
}
func TestHTTPRejectsUntrustedFieldsOriginsQueriesAndOversize(t *testing.T) {
	_, h := apiStore(t)
	for _, body := range []string{`{"modules":["module"],"tenant":"other"}`, `{"modules":["module"]} {}`, strings.Repeat("x", 9000)} {
		r := httptest.NewRequest("POST", "/v1/pairings", strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:10"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/v1/identity", nil)
	r.RemoteAddr = "192.0.2.1:10"
	r.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = request(h, "GET", "/v1/identity?token=secret", "", nil)
	if w.Code != 400 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body.String())
	}
	w = request(h, "GET", "/v1/identity", "", nil)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestHTTPRateLimitIgnoresForwardedAddressAndStaysBounded(t *testing.T) {
	_, h := apiStore(t)
	limited := false
	for i := 0; i < 30; i++ {
		r := httptest.NewRequest("GET", "/v1/identity", nil)
		r.RemoteAddr = "192.0.2.1:10"
		r.Header.Set("X-Forwarded-For", string(rune('a'+i)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("IP limit bypassed")
	}
	health := request(h, "GET", "/healthz", "", nil)
	if health.Code != 200 || !strings.Contains(health.Body.String(), `"signaling":true`) {
		t.Fatal(health.Body.String())
	}
}
