// Package relayprofile owns local Hub credentials and crash-safe refresh state.
// Secrets live in an OS vault; the filesystem contains only empty lock files.
package relayprofile

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/turncred"
)

var (
	ErrNotFound    = errors.New("remote profile not found")
	ErrExists      = errors.New("remote profile already exists; it will not be overwritten")
	ErrVault       = errors.New("remote Keychain unavailable or access denied")
	ErrInvalid     = errors.New("invalid remote profile")
	ErrBusy        = errors.New("another process is updating this remote profile")
	ErrRecovery    = errors.New("remote profile has an unfinished credential transaction; do not retry old credentials; re-enroll using a new profile")
	ErrUnsupported = errors.New("remote Keychain requires macOS with CGO")
)

type Vault interface {
	Load(string) ([]byte, error)
	Create(string, []byte) error
	Replace(string, []byte) error
}
type record struct {
	Version  int             `json:"version"`
	Name     string          `json:"name"`
	RelayURL string          `json:"relay_url"`
	Phase    string          `json:"phase"`
	Seed     []byte          `json:"seed"`
	GrantID  string          `json:"grant_id,omitempty"`
	Tokens   identity.Tokens `json:"tokens"`
}
type Status struct {
	Name                 string             `json:"profile"`
	RelayURL             string             `json:"relay_url"`
	Phase                string             `json:"phase"`
	PublicKeyFingerprint string             `json:"public_key_fingerprint"`
	GrantID              string             `json:"enrollment_grant_id,omitempty"`
	Principal            identity.Principal `json:"principal"`
}
type Store struct {
	vault     Vault
	directory string
	options   relayclient.Options
}

func New(vault Vault, lockDirectory string, options relayclient.Options) *Store {
	return &Store{vault: vault, directory: lockDirectory, options: options}
}
func validName(name string) bool { return callsignal.ID(name) && len(name) <= 64 }
func (s *Store) lock(name string) (func(), error) {
	if s.vault == nil || !validName(name) {
		return nil, ErrInvalid
	}
	return lockProfile(s.directory, name)
}
func (s *Store) lockContext(ctx context.Context, name string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := s.lock(name)
		if !errors.Is(err, ErrBusy) {
			return unlock, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func (s *Store) load(name string) (record, error) {
	data, err := s.vault.Load(name)
	if err != nil {
		return record{}, err
	}
	var r record
	if len(data) > 32768 || callsignal.Decode(data, &r) != nil || r.Version != 1 || r.Name != name || len(r.Seed) != ed25519.SeedSize || (r.Phase != "ready" && r.Phase != "enrolling" && r.Phase != "refresh_pending") {
		return record{}, ErrInvalid
	}
	if r.Phase != "enrolling" && (r.Tokens.Principal.Kind != "hub" || !callsignal.ID(r.Tokens.Principal.ID) || r.Tokens.Principal.HubID != r.Tokens.Principal.ID) {
		return record{}, ErrInvalid
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return record{}, ErrInvalid
	}
	api.Close()
	return r, nil
}
func (s *Store) write(r record, create bool) error {
	data, err := json.Marshal(r)
	if err != nil {
		return ErrInvalid
	}
	if create {
		return s.vault.Create(r.Name, data)
	}
	return s.vault.Replace(r.Name, data)
}
func (s *Store) Enroll(ctx context.Context, name, relayURL string, grant identity.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := s.lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.vault.Load(name); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	api, err := relayclient.New(relayURL, s.options)
	if err != nil {
		return err
	}
	defer api.Close()
	if !grant.ExpiresAt.After(time.Now()) || !callsignal.ID(grant.ID) || len(grant.Secret) != 43 {
		return ErrInvalid
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	r := record{Version: 1, Name: name, RelayURL: strings.TrimSuffix(relayURL, "/"), Phase: "enrolling", Seed: key.Seed(), GrantID: grant.ID}
	if err := s.write(r, true); err != nil {
		return err
	} // key and uncertainty survive an ambiguous enrollment
	tokens, err := api.Enroll(ctx, grant, key)
	if err != nil {
		return err
	}
	r.Tokens, r.Phase = tokens, "ready"
	return s.write(r, false)
}
func (s *Store) Status(name string) (Status, error) {
	unlock, err := s.lock(name)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	r, err := s.load(name)
	if err != nil {
		return Status{}, err
	}
	key := ed25519.NewKeyFromSeed(r.Seed)
	hash := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return Status{Name: name, RelayURL: r.RelayURL, Phase: r.Phase, PublicKeyFingerprint: hex.EncodeToString(hash[:]), GrantID: r.GrantID, Principal: r.Tokens.Principal}, nil
}

// access holds the cross-process lock through challenge, journal, rotation and
// final vault replacement. Each operation reloads current tokens, never a stale
// in-memory refresh token left behind by a different CLI/backend process.
func (s *Store) access(ctx context.Context, name string, force bool) (record, error) {
	if err := ctx.Err(); err != nil {
		return record{}, err
	}
	unlock, err := s.lockContext(ctx, name)
	if err != nil {
		return record{}, err
	}
	defer unlock()
	r, err := s.load(name)
	if err != nil {
		return record{}, err
	}
	if r.Phase != "ready" {
		return record{}, ErrRecovery
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return record{}, err
	}
	defer api.Close()
	credentials, err := relayclient.NewJournaledCredentials(api, r.Tokens, ed25519.NewKeyFromSeed(r.Seed), func() error { r.Phase = "refresh_pending"; return s.write(r, false) }, func(tokens identity.Tokens) error {
		next := r
		next.Phase = "ready"
		next.Tokens = tokens
		if err := s.write(next, false); err != nil {
			return err
		}
		r = next
		return nil
	})
	if err != nil {
		return record{}, err
	}
	if force || !r.Tokens.Principal.AccessExpiresAt.After(time.Now().Add(time.Minute)) {
		if _, err := credentials.RefreshAccess(ctx); err != nil {
			return record{}, err
		}
	}
	return r, nil
}
func (s *Store) RefreshAccess(ctx context.Context, name string) (string, error) {
	r, err := s.access(ctx, name, true)
	if err != nil {
		return "", err
	}
	return r.Tokens.AccessToken, nil
}
func (s *Store) ConnectionConfig(ctx context.Context, name string) (callsignal.ClientConfig, error) {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return callsignal.ClientConfig{}, err
	}
	endpoint, err := url.Parse(r.RelayURL)
	if err != nil {
		return callsignal.ClientConfig{}, ErrInvalid
	}
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	} else {
		endpoint.Scheme = "wss"
	}
	endpoint.Path = "/v1/signaling"
	p := r.Tokens.Principal
	return callsignal.ClientConfig{URL: endpoint.String(), AccessToken: r.Tokens.AccessToken, IdentityID: p.ID, Kind: p.Kind, HubID: p.HubID, AllowInsecureLoopback: s.options.AllowInsecureLoopback, TLSConfig: s.options.TLSConfig}, nil
}
func (s *Store) Pairing(ctx context.Context, name string, modules []string) (identity.Grant, error) {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return identity.Grant{}, err
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return identity.Grant{}, err
	}
	defer api.Close()
	return api.CreatePairing(ctx, r.Tokens.AccessToken, modules)
}
func (s *Store) Installations(ctx context.Context, name string) ([]identity.Installation, error) {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return nil, err
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return nil, err
	}
	defer api.Close()
	return api.Installations(ctx, r.Tokens.AccessToken)
}
func (s *Store) RevokeInstallation(ctx context.Context, name, id string) error {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return err
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return err
	}
	defer api.Close()
	return api.RevokeInstallation(ctx, r.Tokens.AccessToken, id)
}

func (s *Store) ICE(ctx context.Context, name, module string) (turncred.Credentials, error) {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return turncred.Credentials{}, err
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return turncred.Credentials{}, err
	}
	defer api.Close()
	return api.ICE(ctx, r.Tokens.AccessToken, module)
}
