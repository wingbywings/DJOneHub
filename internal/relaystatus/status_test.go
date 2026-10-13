//go:build darwin || linux

package relaystatus

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

func TestPresenceRequiresFreshRuntimeAndValidIdentity(t *testing.T) {
	now := time.Now().UTC()
	s := Snapshot{Version: 1, ObservedAt: now, Running: true, OnlineIdentities: []string{"owned", "revoked", "unrelated"}}
	items := []identity.ManagedIdentity{{ID: "owned"}, {ID: "offline"}, {ID: "revoked", Revoked: true}}
	Observe(s, now).Apply(items)
	if items[0].SignalState != "online" || items[1].SignalState != "offline" || items[2].SignalState != "offline" {
		t.Fatal(items)
	}
	o := Observe(s, now.Add(9*time.Second))
	o.Apply(items)
	if o.State != "stale" || len(o.Snapshot.OnlineIdentities) != 0 || items[0].SignalState != "unknown" || items[0].SignalObservedAt != nil {
		t.Fatal("stale observation inferred connection")
	}
	if Observe(s, now.Add(-2*time.Second)).State != "unavailable" {
		t.Fatal("future snapshot accepted")
	}
}

func TestPrivatePublisherAtomicReadExclusiveOwnershipAndStop(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, Filename)
	read := func(context.Context) Snapshot {
		return Snapshot{Version: 1, ObservedAt: time.Now().UTC(), Running: true, OnlineIdentities: []string{"owned"}}
	}
	stop, err := Start(context.Background(), path, read, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Start(context.Background(), path, read, nil); err == nil {
		second()
		t.Fatal("concurrent publisher accepted")
	}
	if o := Read(path, time.Now()); o.State != "fresh" || !o.Snapshot.Running {
		t.Fatal(o)
	}
	stop()
	if o := Read(path, time.Now()); o.State != "fresh" || o.Snapshot.Running || len(o.Snapshot.OnlineIdentities) != 0 {
		t.Fatal("stop retained online state")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if Read(path, time.Now()).State != "unavailable" {
		t.Fatal("public status file accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".lock", path); err != nil {
		t.Fatal(err)
	}
	if Read(path, time.Now()).State != "unavailable" {
		t.Fatal("symlink accepted")
	}
}
