package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotecall"
)

func TestRemoteJournalDurablePhasesAndReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	b := remotecall.Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 7, InstallationID: "phone"}
	j := openRemoteJournal(path)
	if err := j.begin(b); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"offered", "answer_pending", "active", "ending"} {
		if err := j.phase(b, phase); err != nil {
			t.Fatal(err)
		}
		recovered := openRemoteJournal(path)
		if !recovered.status().RecoveryRequired || recovered.status().Phase != phase {
			t.Fatal("unfinished record ignored", recovered.status())
		}
		if recovered.begin(b) == nil {
			t.Fatal("restart recreated original invitation")
		}
	}
	old := b
	old.Generation++
	if j.phase(old, "active") == nil {
		t.Fatal("old binding edited journal")
	}
	if err := j.confirmedIdle(); err != nil {
		t.Fatal(err)
	}
	if openRemoteJournal(path).status().RecoveryRequired {
		t.Fatal("confirmed end did not persist")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("journal permissions", err)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".remote-call-journal-*"))
	if len(files) != 0 {
		t.Fatal("temporary journal leaked")
	}
	data, _ := os.ReadFile(path)
	for _, secret := range []string{"installation", "phone", "number", "token", "sdp"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("unnecessary journal field", secret)
		}
	}
}

func TestRemoteJournalCorruptionAndWriteFailureFailClosed(t *testing.T) {
	for _, data := range []string{"{", `{"version":2}`, `{"version":1,"unexpected":true}`, strings.Repeat("x", 4097)} {
		path := filepath.Join(t.TempDir(), "journal.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		j := openRemoteJournal(path)
		if !j.status().RecoveryRequired || !j.status().PersistenceFailed {
			t.Fatal("corruption treated as idle")
		}
		if err := j.confirmedIdle(); err != nil {
			t.Fatal(err)
		}
		if openRemoteJournal(path).status().RecoveryRequired {
			t.Fatal("idle did not repair journal")
		}
	}
	path := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	j := &remoteJournal{path: path}
	if j.begin(remotecall.Binding{CallUUID: "call", Generation: 1}) == nil || !j.status().RecoveryRequired {
		t.Fatal("failed journal permitted remote call")
	}
}

func TestRestartJournalBlocksAutomaticAnswerAndSyntheticClear(t *testing.T) {
	_, call := remoteIncoming(t)
	path := filepath.Join(t.TempDir(), "journal.json")
	j := openRemoteJournal(path)
	if err := j.begin(remotecall.Binding{CallUUID: call.UUID, Generation: 7}); err != nil {
		t.Fatal(err)
	}
	if err := j.phase(remotecall.Binding{CallUUID: call.UUID, Generation: 7}, "active"); err != nil {
		t.Fatal(err)
	}
	// A new process cannot prove this CLCC index belongs to the previous UUID.
	restarted := remoteTestApp(t)
	restarted.remoteJournalPath = path
	restarted.ensureRemoteCalls().save(callcontrol.RemoteConfig{Enabled: true, TargetInstallationID: "phone-1", RingTimeoutSeconds: 45, Revision: 1})
	restarted.voiceAgent.mu.Lock()
	restarted.voiceAgent.state.Enabled = true
	restarted.voiceAgent.mu.Unlock()
	restarted.applyObservedCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "active", Number: "10086"}}, time.Now(), true)
	current, _ := restarted.currentCallIdentity()
	if !current.RecoveryRequired || current.Route.Mode != callcontrol.Local || current.UUID == call.UUID {
		t.Fatal("recovered call auto-routed", current)
	}
	if _, err := restarted.executeCallCommand(context.Background(), commandFor(current, "answer")); err == nil {
		t.Fatal("unknown recovered call answered")
	}
	// Disconnect is not a successful idle observation.
	restarted.applyObservedCallPoll(nil, time.Now(), false)
	if !restarted.ensureRemoteJournal().status().RecoveryRequired {
		t.Fatal("USB loss cleared recovery")
	}
	if code := restarted.reserveRemoteDial(callsignal.Message{InstallationID: "phone-1"}, "10086"); code != "recovery_required" {
		t.Fatal(code)
	}
	restarted.applyObservedCallPoll(nil, time.Now(), true)
	if restarted.ensureRemoteJournal().status().RecoveryRequired {
		t.Fatal("confirmed idle stayed blocked")
	}
	if openRemoteJournal(path).status().RecoveryRequired {
		t.Fatal("recovery clear was not durable")
	}
}

func TestRemoteJournalBeforeATAAndFailurePreventsHardware(t *testing.T) {
	a := remoteTestApp(t)
	path := filepath.Join(t.TempDir(), "journal.json")
	a.remoteJournalPath = path
	if err := a.ensureRemoteCalls().save(callcontrol.RemoteConfig{Enabled: true, TargetInstallationID: "phone-1", RingTimeoutSeconds: 45, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	call := startCommandCall(a, "incoming")
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.prepareCallVoice = func(context.Context) error {
		if status := openRemoteJournal(path).status(); status.Phase != "answer_pending" || !status.RecoveryRequired {
			t.Fatal("ATA preparation before durable intent", status)
		}
		return nil
	}
	if _, err := b.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Fail the next phase write without relying on OS permission semantics.
	other := remoteTestApp(t)
	other.remoteJournalPath = filepath.Join(t.TempDir(), "journal.json")
	if err := other.ensureRemoteCalls().save(callcontrol.RemoteConfig{Enabled: true, TargetInstallationID: "phone-1", RingTimeoutSeconds: 45, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	call2 := startCommandCall(other, "incoming")
	b2 := backendFor(other, call2)
	if err := b2.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(other.remoteJournalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other.remoteJournalPath, 0700); err != nil {
		t.Fatal(err)
	}
	other.prepareCallVoice = func(context.Context) error { t.Fatal("hardware prepared after journal failure"); return nil }
	if answered, err := b2.Answer(context.Background()); err == nil || answered {
		t.Fatal("ATA allowed after disk failure", answered, err)
	}
}

func TestCLCCRecoveryRequiresCompleteValidSnapshot(t *testing.T) {
	for _, response := range []string{"", "ERROR", "OK\r\nERROR", "+CLCC: broken\r\nOK", "+CLCC: 1,1,0,0,0", "+CLCC: 1,1,9,0,0\r\nOK", "+CLCC: 1,1,0,0,0,truncated\r\nOK"} {
		if _, err := validatedCallPoll(response); err == nil {
			t.Fatal("unsafe CLCC snapshot", response)
		}
	}
	if calls, err := validatedCallPoll("AT+CLCC\r\nOK\r\n"); err != nil || len(calls) != 0 {
		t.Fatal(calls, err)
	}
	if calls, err := validatedCallPoll("+CLCC: 1,1,0,0,0,\"10086\",129\r\nOK"); err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
}

func TestStartupActiveWithoutJournalAlsoRequiresLocalHandling(t *testing.T) {
	a := remoteTestApp(t)
	a.remoteJournalPath = filepath.Join(t.TempDir(), "journal.json")
	a.demo = false
	a.applyObservedCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "active"}}, time.Now(), true)
	current, _ := a.currentCallIdentity()
	if !current.RecoveryRequired || !openRemoteJournal(a.remoteJournalPath).status().RecoveryRequired {
		t.Fatal("unknown boot-time active call trusted")
	}
	a.applyObservedCallPoll(nil, time.Now(), true)
	a.applyObservedCallPoll([]parsedCall{{Index: 2, Direction: "incoming", State: "incoming"}}, time.Now(), true)
	current, _ = a.currentCallIdentity()
	if current.RecoveryRequired {
		t.Fatal("new call inherited recovery")
	}
	// A normal call first observed after an idle startup is not quarantined.
	b := remoteTestApp(t)
	b.remoteJournalPath = filepath.Join(t.TempDir(), "journal.json")
	b.demo = false
	b.applyObservedCallPoll(nil, time.Now(), true)
	b.applyObservedCallPoll([]parsedCall{{Index: 1, Direction: "outgoing", State: "active"}}, time.Now(), true)
	if current, _ := b.currentCallIdentity(); current.RecoveryRequired {
		t.Fatal("ordinary later call quarantined")
	}
}
