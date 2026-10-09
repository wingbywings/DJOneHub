// Package turncred issues time-limited coturn REST credentials. The shared
// secret never leaves the relay; clients receive only an expiring HMAC proof.
package turncred

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // Required by the coturn REST authentication protocol.
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

const Lifetime = 4 * time.Hour

var ErrConfig = errors.New("invalid TURN configuration")

type Config struct {
	URLs      []string `json:"urls"`
	Secret    string   `json:"secret"`
	RelayOnly bool     `json:"relay_only"`
}
type Server struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}
type Credentials struct {
	Enabled   bool       `json:"enabled"`
	Servers   []Server   `json:"servers"`
	RelayOnly bool       `json:"relay_only"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
type Issuer struct{ config Config }

func ValidURL(value string) bool {
	if len(value) > 512 {
		return false
	}
	scheme, rest, ok := strings.Cut(value, ":")
	if !ok || (scheme != "turn" && scheme != "turns") {
		return false
	}
	address, query, ok := strings.Cut(rest, "?")
	if !ok || (query != "transport=udp" && query != "transport=tcp") || scheme == "turns" && query != "transport=tcp" {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 || host == "" {
		return false
	}
	if net.ParseIP(host) == nil {
		for _, c := range host {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
				return false
			}
		}
	}
	return true
}
func New(config Config) (*Issuer, error) {
	if len(config.Secret) < 32 || len(config.Secret) > 128 || len(config.URLs) == 0 || len(config.URLs) > 4 {
		return nil, ErrConfig
	}
	for _, c := range config.Secret {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, ErrConfig
		}
	}
	seen := map[string]bool{}
	for _, url := range config.URLs {
		if !ValidURL(url) || seen[url] {
			return nil, ErrConfig
		}
		seen[url] = true
	}
	config.URLs = append([]string(nil), config.URLs...)
	return &Issuer{config: config}, nil
}
func (i *Issuer) Issue(now time.Time) (Credentials, error) {
	if i == nil {
		return Credentials{Servers: []Server{}}, nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Credentials{}, err
	}
	expires := now.Add(Lifetime).UTC().Truncate(time.Second)
	username := strconv.FormatInt(expires.Unix(), 10) + ":" + hex.EncodeToString(nonce[:])
	mac := hmac.New(sha1.New, []byte(i.config.Secret))
	mac.Write([]byte(username))
	return Credentials{Enabled: true, RelayOnly: i.config.RelayOnly, ExpiresAt: &expires, Servers: []Server{{URLs: append([]string(nil), i.config.URLs...), Username: username, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil))}}}, nil
}
func (c Credentials) Validate(now time.Time) error {
	if !c.Enabled {
		if len(c.Servers) != 0 || c.RelayOnly || c.ExpiresAt != nil {
			return ErrConfig
		}
		return nil
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.After(now.Add(time.Minute)) || c.ExpiresAt.After(now.Add(Lifetime+time.Minute)) || len(c.Servers) != 1 {
		return ErrConfig
	}
	s := c.Servers[0]
	if len(s.URLs) == 0 || len(s.URLs) > 4 || len(s.Username) > 128 {
		return ErrConfig
	}
	stamp, nonce, ok := strings.Cut(s.Username, ":")
	expires, err := strconv.ParseInt(stamp, 10, 64)
	if !ok || err != nil || expires != c.ExpiresAt.Unix() || len(nonce) != 32 {
		return ErrConfig
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return ErrConfig
	}
	data, err := base64.StdEncoding.DecodeString(s.Credential)
	if err != nil || len(data) != sha1.Size {
		return ErrConfig
	}
	for _, url := range s.URLs {
		if !ValidURL(url) {
			return ErrConfig
		}
	}
	return nil
}
