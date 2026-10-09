package turncred

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestTemporaryCredentialsBoundExpiryAndHideMasterSecret(t *testing.T) {
	secret := strings.Repeat("s", 43)
	issuer, err := New(Config{URLs: []string{"turn:turn.example:3478?transport=udp", "turns:turn.example:5349?transport=tcp"}, Secret: secret, RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c, err := issuer.Issue(now)
	if err != nil || c.Validate(now) != nil || !c.RelayOnly {
		t.Fatal(c, err)
	}
	s := c.Servers[0]
	if s.Credential == secret || strings.Contains(s.Username, secret) {
		t.Fatal("master secret leaked")
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(s.Username))
	if s.Credential != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("coturn REST proof mismatch")
	}
	if c.Validate(now.Add(Lifetime)) == nil {
		t.Fatal("expired credential accepted")
	}
	next, _ := issuer.Issue(now)
	if next.Servers[0].Username == s.Username {
		t.Fatal("call credentials reused")
	}
	c.Servers[0].URLs[0] = "turn:attacker:3478?transport=udp"
	if next.Servers[0].URLs[0] == c.Servers[0].URLs[0] {
		t.Fatal("issuer configuration mutable")
	}
}
func TestTURNURLAndDisabledConfigurationValidation(t *testing.T) {
	for _, url := range []string{"stun:host:3478", "turn:host:3478", "turns:host:5349?transport=udp", "turn:host:0?transport=udp", "turn://user:secret@host:3478?transport=tcp", "turn:host:3478?transport=udp&x=1"} {
		if ValidURL(url) {
			t.Fatal("accepted", url)
		}
	}
	if !ValidURL("turn:[::1]:3478?transport=udp") {
		t.Fatal("IPv6 rejected")
	}
	var issuer *Issuer
	c, err := issuer.Issue(time.Now())
	if err != nil || c.Enabled || c.Validate(time.Now()) != nil {
		t.Fatal(c, err)
	}
	c.RelayOnly = true
	if c.Validate(time.Now()) == nil {
		t.Fatal("disabled configuration enabled relay")
	}
}
