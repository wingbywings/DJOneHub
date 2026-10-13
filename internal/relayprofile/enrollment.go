package relayprofile

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/iniwex5/vohive/internal/callsignal"
	"slices"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

// BeginEnrollment persists the key BEFORE any public registration request is
// submitted. The browser only receives the signed public request.
func (s *Store) BeginEnrollment(ctx context.Context, name, origin, display string, modules []identity.ModuleChoice) (identity.EnrollmentRequest, error) {
	if err := ctx.Err(); err != nil {
		return identity.EnrollmentRequest{}, err
	}
	unlock, err := s.lock(name)
	if err != nil {
		return identity.EnrollmentRequest{}, err
	}
	defer unlock()
	if _, err = s.vault.Load(name); err == nil {
		return identity.EnrollmentRequest{}, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return identity.EnrollmentRequest{}, err
	}
	api, err := relayclient.New(origin, s.options)
	if err != nil {
		return identity.EnrollmentRequest{}, err
	}
	api.Close()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return identity.EnrollmentRequest{}, err
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return identity.EnrollmentRequest{}, err
	}
	now := time.Now().UTC()
	request := identity.EnrollmentRequest{Version: 1, ID: "request_" + base64.RawURLEncoding.EncodeToString(nonce), Name: display, RelayURL: strings.TrimSuffix(origin, "/"), PublicKey: pub, Modules: modules, CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	request.Signature = ed25519.Sign(key, identity.EnrollmentRequestMessage(request))
	if err = request.Validate(now); err != nil {
		return identity.EnrollmentRequest{}, ErrInvalid
	}
	r := record{Version: 1, Name: name, RelayURL: request.RelayURL, Phase: "request_pending", Seed: key.Seed(), Request: &request}
	if err = s.write(r, true); err != nil {
		return identity.EnrollmentRequest{}, err
	}
	return request, nil
}
func (s *Store) EnrollmentRequest(name string) (identity.EnrollmentRequest, error) {
	unlock, err := s.lock(name)
	if err != nil {
		return identity.EnrollmentRequest{}, err
	}
	defer unlock()
	r, err := s.load(name)
	if err != nil {
		return identity.EnrollmentRequest{}, err
	}
	if r.Request == nil {
		return identity.EnrollmentRequest{}, ErrInvalid
	}
	return *r.Request, nil
}
func (s *Store) SubmitEnrollment(ctx context.Context, name string) (identity.EnrollmentRequestStatus, error) {
	unlock, err := s.lockContext(ctx, name)
	if err != nil {
		return identity.EnrollmentRequestStatus{}, err
	}
	defer unlock()
	r, err := s.load(name)
	if err != nil {
		return identity.EnrollmentRequestStatus{}, err
	}
	if r.Phase != "request_pending" || r.Request == nil || r.Request.Validate(time.Now()) != nil {
		return identity.EnrollmentRequestStatus{}, ErrInvalid
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return identity.EnrollmentRequestStatus{}, err
	}
	defer api.Close()
	return api.SubmitEnrollmentRequest(ctx, *r.Request)
}
func (s *Store) CompleteEnrollment(ctx context.Context, name string, file identity.EnrollmentAuthorization) error {
	unlock, err := s.lockContext(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := s.load(name)
	if err != nil {
		return err
	}
	if r.Phase != "request_pending" {
		return ErrRecovery
	}
	key := ed25519.NewKeyFromSeed(r.Seed)
	now := time.Now()
	if !callsignal.ID(file.Grant.ID) || !callsignal.ID(file.Grant.Secret) || len(file.Grant.Secret) != 43 || r.Request == nil || r.Request.Validate(now) != nil || file.Version != 2 || file.RequestID != r.Request.ID || strings.TrimSuffix(file.RelayURL, "/") != r.RelayURL || file.PublicKeyFingerprint != identity.KeyFingerprint(key.Public().(ed25519.PublicKey)) || !file.Grant.ExpiresAt.After(now) || file.Grant.ExpiresAt.After(now.Add(10*time.Minute+time.Second)) || file.Grant.ExpiresAt.After(r.Request.ExpiresAt) || len(file.Grant.Modules) == 0 {
		return ErrInvalid
	}
	selected := map[string]bool{}
	for _, m := range r.Request.Modules {
		selected[m.ID] = true
	}
	seen := map[string]bool{}
	for _, m := range file.Grant.Modules {
		if !selected[m] || seen[m] {
			return ErrInvalid
		}
		seen[m] = true
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return err
	}
	defer api.Close()
	r.Phase = "enrolling"
	r.GrantID = file.Grant.ID
	if err = s.write(r, false); err != nil {
		return err
	}
	tokens, err := api.Enroll(ctx, file.Grant, key)
	if err != nil {
		return err
	}
	expected := append([]string(nil), file.Grant.Modules...)
	slices.Sort(expected)
	actual := append([]string(nil), tokens.Principal.Modules...)
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		return ErrInvalid
	}
	r.Phase = "ready"
	r.Tokens = tokens
	return s.write(r, false)
}
