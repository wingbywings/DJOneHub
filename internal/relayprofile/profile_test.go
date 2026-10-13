//go:build cgo && (darwin || linux)

package relayprofile

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

// Models atomic vault replacement across newly constructed Store instances.
// No unit test reads or writes the user's real login Keychain.
type memoryVault struct {
	mu   sync.Mutex
	data map[string][]byte
	fail func(record) bool
}

func (v *memoryVault) Load(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, ok := v.data[name]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(data), nil
}
func (v *memoryVault) Create(name string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.data == nil {
		v.data = make(map[string][]byte)
	}
	if v.data[name] != nil {
		return ErrExists
	}
	v.data[name] = bytes.Clone(data)
	return nil
}
func (v *memoryVault) Replace(name string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.data[name] == nil {
		return ErrNotFound
	}
	var r record
	json.Unmarshal(data, &r)
	if v.fail != nil && v.fail(r) {
		return ErrVault
	}
	v.data[name] = bytes.Clone(data)
	return nil
}

type fixture struct {
	store   *Store
	vault   *memoryVault
	remote  *identity.Store
	server  *httptest.Server
	options relayclient.Options
	grant   identity.Grant
}

func setup(t *testing.T, wrap func(http.Handler) http.Handler) *fixture {
	t.Helper()
	remote, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	h := relay.NewServer(remote, relay.WithEnrollmentOrigin(origin))
	var handler http.Handler = h
	if wrap != nil {
		handler = wrap(h)
	}
	server.Config.Handler = handler
	server.StartTLS()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	options := relayclient.Options{TLSConfig: &tls.Config{RootCAs: roots}}
	vault := &memoryVault{}
	store := New(vault, filepath.Join(t.TempDir(), "locks"), options)
	grant, err := remote.ProvisionEnrollment(context.Background(), "tenant", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(); server.Close(); remote.Close() })
	return &fixture{store, vault, remote, server, options, grant}
}
func (f *fixture) enroll(t *testing.T) {
	t.Helper()
	if err := f.store.Enroll(context.Background(), "default", f.server.URL, f.grant); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) restart() *Store { return New(f.vault, f.store.directory, f.options) }

func TestProfileEnrollmentReloadRefreshAndSecretBoundaries(t *testing.T) {
	f := setup(t, nil)
	f.enroll(t)
	r, err := f.store.load("default")
	if err != nil {
		t.Fatal(err)
	}
	config, err := f.restart().ConnectionConfig(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := callsignal.Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	ws.Close()
	token, err := f.restart().RefreshAccess(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.restart().load("default")
	if err != nil {
		t.Fatal(err)
	}
	if next.Phase != "ready" || next.Tokens.AccessToken != token || next.Tokens.RefreshToken == r.Tokens.RefreshToken {
		t.Fatal("rotation not stored")
	}
	status, err := f.store.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(status)
	for _, secret := range []string{r.Tokens.RefreshToken, r.Tokens.AccessToken, next.Tokens.RefreshToken, next.Tokens.AccessToken} {
		if bytes.Contains(metadata, []byte(secret)) {
			t.Fatal("status leaked credential")
		}
	}
	entries, err := os.ReadDir(f.store.directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("secret files written")
	}
	info, _ := entries[0].Info()
	if info.Size() != 0 || info.Mode().Perm() != 0600 {
		t.Fatal("lock file contains data or has unsafe permissions")
	}
	if err := f.store.Enroll(context.Background(), "default", f.server.URL, f.grant); !errors.Is(err, ErrExists) {
		t.Fatal("enrollment overwrote identity", err)
	}
}
func TestRefreshJournalSurvivesLostResponseAndRestart(t *testing.T) {
	var rotations atomic.Int32
	f := setup(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/auth/refresh" {
				next.ServeHTTP(w, r)
				return
			}
			rotations.Add(1)
			record := httptest.NewRecorder()
			next.ServeHTTP(record, r)
			w.WriteHeader(record.Code)
			w.Write([]byte("{"))
		})
	})
	f.enroll(t)
	if _, err := f.store.RefreshAccess(context.Background(), "default"); err == nil {
		t.Fatal("lost response succeeded")
	}
	status, err := f.restart().Status("default")
	if err != nil || status.Phase != "refresh_pending" {
		t.Fatal(status.Phase, err)
	}
	if _, err := f.restart().ConnectionConfig(context.Background(), "default"); !errors.Is(err, ErrRecovery) {
		t.Fatal("restart used ambiguous credentials", err)
	}
	if _, err := f.restart().RefreshAccess(context.Background(), "default"); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
	if rotations.Load() != 1 {
		t.Fatal("restart replayed rotation")
	}
}
func TestJournalWriteFailurePreventsRequestAndFinalSaveFailureStaysPending(t *testing.T) {
	var rotations atomic.Int32
	f := setup(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth/refresh" {
				rotations.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	f.enroll(t)
	f.vault.fail = func(r record) bool { return r.Phase == "refresh_pending" }
	if _, err := f.store.RefreshAccess(context.Background(), "default"); !errors.Is(err, relayclient.ErrPersistence) {
		t.Fatal(err)
	}
	if rotations.Load() != 0 {
		t.Fatal("sent rotation before journal persisted")
	}
	f.vault.fail = func(r record) bool { return r.Phase == "ready" }
	if _, err := f.restart().RefreshAccess(context.Background(), "default"); !errors.Is(err, relayclient.ErrPersistence) {
		t.Fatal(err)
	}
	if _, err := f.restart().RefreshAccess(context.Background(), "default"); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
	if rotations.Load() != 1 {
		t.Fatal("final save failure replayed operation")
	}
}
func TestEnrollmentJournalAndOfflineFingerprintRecovery(t *testing.T) {
	f := setup(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/hubs/enroll" {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			w.WriteHeader(rec.Code)
			w.Write([]byte("{"))
		})
	})
	if err := f.store.Enroll(context.Background(), "default", f.server.URL, f.grant); err == nil {
		t.Fatal("ambiguous enrollment succeeded")
	}
	status, err := f.restart().Status("default")
	if err != nil || status.Phase != "enrolling" {
		t.Fatal(err)
	}
	items, err := f.remote.AdminLookupFingerprint(context.Background(), status.PublicKeyFingerprint)
	if err != nil || len(items) != 1 {
		t.Fatal("cannot find orphaned enrollment", err)
	}
	if err := f.remote.AdminRevoke(context.Background(), items[0].ID); err != nil {
		t.Fatal(err)
	}
	items, err = f.remote.AdminLookupFingerprint(context.Background(), status.PublicKeyFingerprint)
	if err != nil || !items[0].Revoked {
		t.Fatal("offline recovery revoke failed", err)
	}
	if _, err := f.restart().ConnectionConfig(context.Background(), "default"); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
}
func TestProfileLockSerializesSeparateStoreInstances(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var rotations atomic.Int32
	f := setup(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth/refresh" && rotations.Add(1) == 1 {
				close(entered)
				<-release
			}
			next.ServeHTTP(w, r)
		})
	})
	f.enroll(t)
	done := make(chan error, 1)
	go func() { _, err := f.store.RefreshAccess(context.Background(), "default"); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := f.restart().RefreshAccess(ctx, "default")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("two refresh processes overlapped", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := f.restart().RefreshAccess(context.Background(), "default"); err != nil {
		t.Fatal("did not reload latest committed credential", err)
	}
}
func TestStoredProfilePairListAndRevoke(t *testing.T) {
	f := setup(t, nil)
	f.enroll(t)
	g, err := f.store.Pairing(context.Background(), "default", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	pub, key, _ := ed25519.GenerateKey(nil)
	proof := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("pairing", proof))
	phone, err := f.remote.Redeem(context.Background(), proof)
	if err != nil {
		t.Fatal(err)
	}
	items, err := f.store.Installations(context.Background(), "default")
	if err != nil || len(items) != 1 || items[0].ID != phone.Principal.ID {
		t.Fatal(items, err)
	}
	if err := f.store.RevokeInstallation(context.Background(), "default", phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.remote.Authenticate(context.Background(), phone.AccessToken); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal(err)
	}
}
func TestPrivateInputOutputAndLockPathProtection(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "grant.json")
	data := map[string]string{"id": "grant"}
	if err := WritePrivateJSON(path, data); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateJSON(path, map[string]string{"id": "replacement"}); err == nil {
		t.Fatal("private output overwritten")
	}
	var got map[string]string
	if err := ReadPrivateJSON(path, &got); err != nil || got["id"] != "grant" {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.json")
	os.Symlink(path, link)
	if ReadPrivateJSON(link, &got) == nil {
		t.Fatal("symlink credential accepted")
	}
	os.Chmod(path, 0644)
	if ReadPrivateJSON(path, &got) == nil {
		t.Fatal("public credential accepted")
	}
	os.Symlink(path, filepath.Join(directory, "default.lock"))
	if unlock, err := lockProfile(directory, "default"); err == nil {
		unlock()
		t.Fatal("symlink lock accepted")
	}
	if _, err := New(&memoryVault{}, directory, relayclient.Options{}).Status("../default"); !errors.Is(err, ErrInvalid) {
		t.Fatal("profile path traversal", err)
	}
}
