// Package relayclient implements native identity requests. It never logs keys
// or credentials, and does not choose a persistence location on the caller's behalf.
package relayclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/turncred"
)

var (
	ErrRequest     = errors.New("relay identity request failed")
	ErrCredentials = errors.New("relay credentials invalid; enrollment or pairing required")
	ErrPersistence = errors.New("rotated relay credentials could not be saved; pairing recovery required")
)

type Client struct {
	base string
	http *http.Client
}
type Options struct {
	AllowInsecureLoopback bool
	TLSConfig             *tls.Config
}

func New(base string, options Options) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, ErrRequest
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !options.AllowInsecureLoopback || ip == nil || !ip.IsLoopback() {
			return nil, ErrRequest
		}
	}
	var tlsConfig *tls.Config
	if options.TLSConfig != nil {
		if options.TLSConfig.InsecureSkipVerify {
			return nil, ErrRequest
		}
		tlsConfig = options.TLSConfig.Clone()
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 4, MaxIdleConnsPerHost: 2}
	return &Client{base: strings.TrimSuffix(base, "/"), http: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRequest }}}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) post(ctx context.Context, path string, body, result any) error {
	return c.request(ctx, http.MethodPost, path, "", body, result)
}
func (c *Client) request(ctx context.Context, method, path, token string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return ErrRequest
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return ErrRequest
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(r)
	if err != nil {
		return ErrRequest
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrRequest
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 32769))
	if err != nil || len(data) > 32768 {
		return ErrRequest
	}
	if callsignal.Decode(data, result) != nil {
		return ErrRequest
	}
	return nil
}
func (c *Client) CreatePairing(ctx context.Context, access string, modules []string) (identity.Grant, error) {
	var result identity.Grant
	err := c.request(ctx, http.MethodPost, "/v1/pairings", access, map[string]any{"modules": modules}, &result)
	if err == nil && (!callsignal.ID(result.ID) || len(result.Secret) != 43 || !result.ExpiresAt.After(time.Now())) {
		err = ErrRequest
	}
	return result, err
}
func (c *Client) Installations(ctx context.Context, access string) ([]identity.Installation, error) {
	// This endpoint returns an array, unlike the object-only signing protocol.
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/installations", nil)
	if err != nil {
		return nil, ErrRequest
	}
	r.Header.Set("Authorization", "Bearer "+access)
	response, err := c.http.Do(r)
	if err != nil {
		return nil, ErrRequest
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrRequest
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32769))
	if err != nil || len(data) > 32768 {
		return nil, ErrRequest
	}
	var items []identity.Installation
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&items) != nil || d.Decode(new(any)) != io.EOF || len(items) > 64 {
		return nil, ErrRequest
	}
	return items, nil
}
func (c *Client) RevokeInstallation(ctx context.Context, access, id string) error {
	if !callsignal.ID(id) {
		return ErrRequest
	}
	var result struct {
		Revoked bool `json:"revoked"`
	}
	if err := c.request(ctx, http.MethodDelete, "/v1/installations/"+id, access, nil, &result); err != nil {
		return err
	}
	if !result.Revoked {
		return ErrRequest
	}
	return nil
}
func (c *Client) Enroll(ctx context.Context, grant identity.Grant, key ed25519.PrivateKey) (identity.Tokens, error) {
	return c.redeem(ctx, "enrollment", "/v1/hubs/enroll", grant, key)
}
func (c *Client) Pair(ctx context.Context, grant identity.Grant, key ed25519.PrivateKey) (identity.Tokens, error) {
	return c.redeem(ctx, "pairing", "/v1/pairings/redeem", grant, key)
}
func (c *Client) redeem(ctx context.Context, kind, path string, grant identity.Grant, key ed25519.PrivateKey) (identity.Tokens, error) {
	if len(key) != ed25519.PrivateKeySize || !callsignal.ID(grant.ID) || len(grant.Secret) != 43 {
		return identity.Tokens{}, ErrCredentials
	}
	proof := identity.GrantProof{ID: grant.ID, Secret: grant.Secret, PublicKey: key.Public().(ed25519.PublicKey)}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage(kind, proof))
	var token identity.Tokens
	if err := c.post(ctx, path, proof, &token); err != nil {
		return identity.Tokens{}, err
	}
	if !valid(token) || kind == "enrollment" && token.Principal.Kind != "hub" || kind == "pairing" && token.Principal.Kind != "installation" {
		return identity.Tokens{}, ErrCredentials
	}
	return token, nil
}
func valid(token identity.Tokens) bool {
	p := token.Principal
	if !callsignal.ID(p.ID) || !callsignal.ID(p.HubID) || !callsignal.ID(p.Tenant) || (p.Kind != "hub" && p.Kind != "installation") || p.Version == 0 || len(p.Modules) == 0 || len(p.Modules) > 32 {
		return false
	}
	if p.Kind == "hub" && p.ID != p.HubID {
		return false
	}
	seen := make(map[string]bool)
	for _, id := range p.Modules {
		if !callsignal.ID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return len(token.AccessToken) == 43 && callsignal.ID(token.AccessToken) && len(token.RefreshToken) == 43 && callsignal.ID(token.RefreshToken) && p.AccessExpiresAt.After(time.Now()) && token.RefreshExpiresAt.After(time.Now())
}

// Credentials serializes the refresh transaction. Save must atomically persist
// the full token set in protected storage and return only after it is durable.
// The private key must already be stored by the caller. There is no automatic
// retry after an ambiguous rotation response or a failed save.
type Credentials struct {
	mu           sync.Mutex
	api          *Client
	key          ed25519.PrivateKey
	tokens       identity.Tokens
	save         func(identity.Tokens) error
	beginRefresh func() error
	blocked      bool
}

// NewJournaledCredentials records uncertainty durably BEFORE issuing a rotation.
// The caller's save callback must atomically replace that marker with new tokens.
func NewJournaledCredentials(api *Client, tokens identity.Tokens, key ed25519.PrivateKey, begin func() error, save func(identity.Tokens) error) (*Credentials, error) {
	if begin == nil {
		return nil, ErrCredentials
	}
	c, err := NewCredentials(api, tokens, key, save)
	if err != nil {
		return nil, err
	}
	c.beginRefresh = begin
	return c, nil
}

func NewCredentials(api *Client, tokens identity.Tokens, key ed25519.PrivateKey, save func(identity.Tokens) error) (*Credentials, error) {
	// Expired access is allowed on load: the still-valid refresh credential can
	// recover it before a WebSocket is opened.
	check := tokens
	check.Principal.AccessExpiresAt = time.Now().Add(time.Minute)
	if api == nil || !valid(check) || len(key) != ed25519.PrivateKeySize || save == nil {
		return nil, ErrCredentials
	}
	tokens.Principal.Modules = slices.Clone(tokens.Principal.Modules)
	return &Credentials{api: api, key: slices.Clone(key), tokens: tokens, save: save}, nil
}

// RefreshAccess plugs directly into callsignal.Client.MaintainAuthorization.
func (c *Credentials) RefreshAccess(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked {
		return "", ErrCredentials
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	old := c.tokens
	var challenge identity.Challenge
	if err := c.api.post(ctx, "/v1/auth/challenge", map[string]string{"identity_id": old.Principal.ID, "refresh_token": old.RefreshToken}, &challenge); err != nil {
		return "", err
	}
	if !callsignal.ID(challenge.ID) || len(challenge.Value) != 43 || !callsignal.ID(challenge.Value) || !challenge.ExpiresAt.After(time.Now()) {
		return "", ErrRequest
	}
	proof := identity.RefreshProof{IdentityID: old.Principal.ID, RefreshToken: old.RefreshToken, ChallengeID: challenge.ID, Challenge: challenge.Value}
	proof.Signature = ed25519.Sign(c.key, identity.RefreshMessage(proof))
	var next identity.Tokens
	// From this point any failure can leave the old refresh credential spent.
	// Only a valid response followed by durable save clears the blocked state.
	c.blocked = true
	if c.beginRefresh != nil {
		if err := c.beginRefresh(); err != nil {
			return "", ErrPersistence
		}
	}
	if err := c.api.post(ctx, "/v1/auth/refresh", proof, &next); err != nil {
		return "", err
	}
	p, q := old.Principal, next.Principal
	if !valid(next) || p.ID != q.ID || p.Kind != q.Kind || p.HubID != q.HubID || p.Tenant != q.Tenant || p.Version != q.Version || !slices.Equal(p.Modules, q.Modules) || old.RefreshToken == next.RefreshToken {
		return "", ErrCredentials
	}
	saved := next
	saved.Principal.Modules = slices.Clone(next.Principal.Modules)
	if err := c.save(saved); err != nil {
		return "", ErrPersistence
	}
	c.tokens = next
	c.blocked = false
	return next.AccessToken, nil
}

func (c *Client) ICE(ctx context.Context, access, module string) (turncred.Credentials, error) {
	var result turncred.Credentials
	if !callsignal.ID(module) {
		return result, ErrCredentials
	}
	if err := c.request(ctx, http.MethodPost, "/v1/ice", access, map[string]string{"module_id": module}, &result); err != nil {
		return result, err
	}
	if err := result.Validate(time.Now()); err != nil {
		return turncred.Credentials{}, err
	}
	return result, nil
}
