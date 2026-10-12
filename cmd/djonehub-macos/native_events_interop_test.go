//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in: exercise URLSession WebSocket against the actual authenticated Go
// listener, with an ephemeral bootstrap and no modem, microphone or UAC access.
func TestNativeEventsSwiftInterop(t *testing.T) {
	if os.Getenv("DJONEHUB_NATIVE_EVENTS_INTEROP") != "1" {
		t.Skip("opt-in native WebSocket interop")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "bootstrap.json")
	events := &nativeEvents{}
	closeServer, err := startNativeControl(path, func(string) (*app, bool) { t.Error("event test accessed hardware config"); return nil, false }, events)
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() {
		timer := time.NewTicker(100 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				events.publish()
			}
		}
	}()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "swift", "test", "--package-path", filepath.Join(cwd, "../../macos/DJOneHubAudioHost"), "--filter", "NativeControlTests/testLiveNativeEventsAndCancellation")
	command.Env = append(os.Environ(), "DJONEHUB_TEST_NATIVE_BOOTSTRAP="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Swift native interop: %v\n%s", err, output)
	}
	t.Log("Swift URLSession received authenticated native changes and cancelled its subscription")
}
