//go:build darwin || linux

package turncred

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateConfigRejectsPublicPermissionsAndSymlinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn.json")
	data := []byte(`{"urls":["turn:relay.example:3478?transport=udp"],"secret":"testonlytestonlytestonlytestonly123","relay_only":true}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("public master secret accepted")
	}
}
