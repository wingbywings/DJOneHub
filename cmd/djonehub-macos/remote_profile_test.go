//go:build cgo && (darwin || linux)

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/relayprofile"
)

type commandVault map[string][]byte

func (v commandVault) Load(name string) ([]byte, error) {
	data, ok := v[name]
	if !ok {
		return nil, relayprofile.ErrNotFound
	}
	return bytes.Clone(data), nil
}
func (v commandVault) Create(name string, data []byte) error {
	if v[name] != nil {
		return relayprofile.ErrExists
	}
	v[name] = bytes.Clone(data)
	return nil
}
func (v commandVault) Replace(name string, data []byte) error {
	if v[name] == nil {
		return relayprofile.ErrNotFound
	}
	v[name] = bytes.Clone(data)
	return nil
}

func TestRemoteCommandsEnrollPairCheckAndRevoke(t *testing.T) {
	ctx := context.Background()
	database, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	h := relay.NewServer(database)
	server := httptest.NewTLSServer(h)
	defer server.Close()
	defer h.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	vault := commandVault{}
	locks := filepath.Join(t.TempDir(), "locks")
	factory := func(options relayclient.Options) (*relayprofile.Store, error) {
		options.TLSConfig = &tls.Config{RootCAs: roots}
		return relayprofile.New(vault, locks, options), nil
	}
	private := filepath.Join(t.TempDir(), "private")
	os.Mkdir(private, 0700)
	grant, err := database.ProvisionEnrollment(ctx, "tenant", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(private, "enrollment.json")
	if err := relayprofile.WritePrivateJSON(input, grant); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runRemoteCommand(ctx, args, &out, factory); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), grant.Secret) {
			t.Fatal("enrollment secret printed")
		}
		return out.String()
	}
	run("enroll", "--relay", server.URL, "--enrollment-file", input)
	if len(vault) != 1 {
		t.Fatal("identity not stored in vault")
	}
	var status relayprofile.Status
	if err := json.Unmarshal([]byte(run("status")), &status); err != nil {
		t.Fatal(err)
	}
	if status.Phase != "ready" || status.Principal.Kind != "hub" {
		t.Fatal("status not ready")
	}
	run("check")
	output := filepath.Join(private, "pairing.json")
	run("pair", "--modules", "module", "--out", output)
	var pairing struct {
		Version  int            `json:"version"`
		RelayURL string         `json:"relay_url"`
		Grant    identity.Grant `json:"grant"`
	}
	if err := relayprofile.ReadPrivateJSON(output, &pairing); err != nil {
		t.Fatal(err)
	}
	if pairing.Version != 1 || pairing.RelayURL != server.URL {
		t.Fatal("pairing metadata")
	}
	pub, key, _ := ed25519.GenerateKey(nil)
	proof := identity.GrantProof{ID: pairing.Grant.ID, Secret: pairing.Grant.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("pairing", proof))
	phone, err := database.Redeem(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	list := run("installations")
	if !strings.Contains(list, phone.Principal.ID) || strings.Contains(list, phone.AccessToken) {
		t.Fatal("installation metadata")
	}
	run("revoke", "--installation", phone.Principal.ID)
	if _, err := database.Authenticate(ctx, phone.AccessToken); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal(err)
	}
	if run("installations") != "[]\n" {
		t.Fatal("revoked installation listed")
	}
	if err := runRemoteCommand(ctx, []string{"enroll", "--relay", server.URL, "--enrollment-file", input}, &bytes.Buffer{}, factory); !errors.Is(err, relayprofile.ErrExists) {
		t.Fatal("profile overwritten", err)
	}
	data, _ := os.ReadFile(output)
	if err := runRemoteCommand(ctx, []string{"pair", "--modules", "module", "--out", output}, &bytes.Buffer{}, factory); err == nil {
		t.Fatal("pairing output overwritten")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(data, after) {
		t.Fatal("existing pairing changed")
	}
}
func TestRemoteCommandHelpAndInvalidFlagsNeverOpenKeychain(t *testing.T) {
	factory := func(relayclient.Options) (*relayprofile.Store, error) {
		t.Fatal("unexpected Keychain access")
		return nil, nil
	}
	var out bytes.Buffer
	if err := runRemoteCommand(context.Background(), []string{"--help"}, &out, factory); err != nil || !strings.Contains(out.String(), "enroll") {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"unknown"}, {"status", "--secret-token", "do-not-print"}, {"status", "--relay", "https://secret.example"}, {"status", "--profile", "../escape"}, {"media-check"}, {"media-check", "--module", "../escape"}, {"check", "--module", "module"}, {"media-check", "--modules", "module"}} {
		out.Reset()
		err := runRemoteCommand(context.Background(), args, &out, factory)
		if err == nil {
			t.Fatal("invalid arguments accepted")
		}
		if strings.Contains(err.Error()+out.String(), "do-not-print") || strings.Contains(err.Error()+out.String(), "secret.example") {
			t.Fatal("untrusted flag values echoed")
		}
	}
}
