// Package voippush sends incoming-call wakeups. APNs acceptance is never an
// acknowledgement that CallKit reported, or that the user answered, a call.
package voippush

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
)

var ErrConfig = errors.New("invalid private APNs configuration")
var ErrDelivery = errors.New("APNs did not accept incoming call")

type Config struct {
	TeamID      string `json:"team_id"`
	KeyID       string `json:"key_id"`
	BundleID    string `json:"bundle_id"`
	Environment string `json:"environment"`
	PrivateKey  string `json:"private_key_pem"`
}
type Result struct {
	InvalidToken  bool
	InvalidatedAt time.Time
}
type Sender interface {
	Environment() string
	Send(context.Context, string, callsignal.Message) (Result, error)
}
type Client struct {
	config   Config
	key      *ecdsa.PrivateKey
	http     *http.Client
	endpoint string
	mu       sync.Mutex
	jwt      string
	issued   time.Time
}

func identifier(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func ValidToken(s string) bool {
	// Device token lengths are not fixed by Apple. Keep a generous bounded hex address.
	if len(s) < 32 || len(s) > 1024 || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func New(config Config) (*Client, error) {
	if !identifier(config.TeamID, 10) || !identifier(config.KeyID, 10) || len(config.BundleID) > 200 || !strings.Contains(config.BundleID, ".") {
		return nil, ErrConfig
	}
	for _, c := range config.BundleID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return nil, ErrConfig
		}
	}
	endpoint := "https://api.push.apple.com"
	if config.Environment == "sandbox" {
		endpoint = "https://api.sandbox.push.apple.com"
	} else if config.Environment != "production" {
		return nil, ErrConfig
	}
	block, rest := pem.Decode([]byte(config.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrConfig
	}
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrConfig
	}
	key, ok := raw.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, ErrConfig
	}
	config.PrivateKey = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true
	return &Client{config: config, key: key, endpoint: endpoint, http: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Environment() string { return c.config.Environment }
func (c *Client) authorization(now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jwt != "" && now.Sub(c.issued) >= 0 && now.Sub(c.issued) < 20*time.Minute {
		return c.jwt, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.config.KeyID})
	body, _ := json.Marshal(map[string]any{"iss": c.config.TeamID, "iat": now.Unix()})
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(message))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, digest[:])
	if err != nil {
		return "", ErrDelivery
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	c.jwt = message + "." + base64.RawURLEncoding.EncodeToString(signature)
	c.issued = now
	return c.jwt, nil
}
func (c *Client) Send(ctx context.Context, token string, invite callsignal.Message) (Result, error) {
	var call callsignal.Invite
	if !ValidToken(token) || invite.Type != "invite" || invite.Validate("hub") != nil || callsignal.Decode(invite.Payload, &call) != nil || call.Direction == "outgoing" || !call.Deadline.After(time.Now()) {
		return Result{}, ErrDelivery
	}
	payload, _ := json.Marshal(struct {
		APS    map[string]any     `json:"aps"`
		Invite callsignal.Message `json:"invite"`
	}{map[string]any{}, invite})
	if len(payload) > 5120 {
		return Result{}, ErrDelivery
	}
	jwt, err := c.authorization(time.Now())
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, call.Deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/3/device/"+token, bytes.NewReader(payload))
	if err != nil {
		return Result{}, ErrDelivery
	}
	req.Header.Set("Authorization", "bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apns-topic", c.config.BundleID+".voip")
	req.Header.Set("apns-push-type", "voip")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-expiration", "0")
	req.Header.Set("apns-id", invite.CallUUID)
	// No retry or collapse: an uncertain send must not create another call UI.
	response, err := c.http.Do(req)
	if err != nil {
		return Result{}, ErrDelivery
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		return Result{}, ErrDelivery
	}
	if response.StatusCode == 200 {
		return Result{}, nil
	}
	var failure struct {
		Reason    string `json:"reason"`
		Timestamp int64  `json:"timestamp"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure) != nil {
		return Result{}, ErrDelivery
	}
	result := Result{}
	if response.StatusCode == 400 && (failure.Reason == "BadDeviceToken" || failure.Reason == "DeviceTokenNotForTopic") {
		result.InvalidToken = true
	}
	if response.StatusCode == 410 && failure.Reason == "Unregistered" && failure.Timestamp > 0 {
		result.InvalidToken = true
		result.InvalidatedAt = time.UnixMilli(failure.Timestamp)
	}
	return result, ErrDelivery
}
