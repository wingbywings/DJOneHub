//go:build cgo

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

func TestOfflineProvisionAndRevoke(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	output := filepath.Join(directory, "enrollment.json")
	args := []string{"provision", "--state-dir", directory, "--tenant", "test", "--modules", "module-a", "--out", output}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("enrollment output must be private", err)
	}
	if err := run(args); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing enrollment overwritten", err)
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("failed provision changed enrollment output", err)
	}
	var grant identity.Grant
	if err := json.Unmarshal(data, &grant); err != nil {
		t.Fatal(err)
	}
	store, err := identity.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	proof := identity.GrantProof{ID: grant.ID, Secret: grant.Secret, PublicKey: public}
	proof.Signature = ed25519.Sign(private, identity.GrantMessage("enrollment", proof))
	hub, err := store.Enroll(context.Background(), proof)
	if err != nil {
		t.Fatal("CLI output cannot enroll Hub", err)
	}
	if err := run([]string{"revoke", "--state-dir", directory, "--id", hub.Principal.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(context.Background(), hub.AccessToken); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("CLI revoke did not invalidate Hub", err)
	}
}
