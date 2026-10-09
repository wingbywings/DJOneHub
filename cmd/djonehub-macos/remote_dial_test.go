package main

import (
	"context"
	"errors"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotecall"
	"net/http"
	"strings"
	"testing"
	"time"
)

func remoteOutgoing(t *testing.T) (*app, callRecord) {
	t.Helper()
	a := remoteTestApp(t)
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	m := callsignal.Message{CallUUID: callcontrol.NewCallUUID(), InstallationID: "phone-1"}
	if code := a.reserveRemoteDial(m, "12345"); code != "" {
		t.Fatal(code)
	}
	call, _ := a.currentCallIdentity()
	return a, call
}

func TestRemoteOutgoingRejectsPolicyAndKeepsPlaceholderUntilDial(t *testing.T) {
	a, call := remoteOutgoing(t)
	a.applyCallPoll(nil, time.Now().Add(20*time.Second))
	if !a.callMatches(call.UUID) {
		t.Fatal("idle poll removed preparing dial")
	}
	a.discardRemoteDial(call.UUID)
	if a.hasActiveCall() {
		t.Fatal("cancelled placeholder retained")
	}
	request := callsignal.Message{CallUUID: callcontrol.NewCallUUID(), InstallationID: "wrong-phone"}
	if a.reserveRemoteDial(request, "12345") != "dial_not_allowed" {
		t.Fatal("foreign installation dialed")
	}
	request.InstallationID = "phone-1"
	for _, bad := range []string{"+", "123;ATH", "*123#", strings.Repeat("1", 33), "12\n3"} {
		if a.reserveRemoteDial(request, bad) != "invalid_number" {
			t.Fatal("invalid number accepted", bad)
		}
	}
	a.ensureVoiceAgent().mu.Lock()
	a.ensureVoiceAgent().state.Enabled = true
	a.ensureVoiceAgent().mu.Unlock()
	if a.reserveRemoteDial(request, "12345") != "dial_not_allowed" {
		t.Fatal("AI policy bypassed")
	}
}

func TestRemoteOutgoingConnectsOnlyAfterSystemAudioAndHangupReleasesLease(t *testing.T) {
	a, call := remoteOutgoing(t)
	a.moduleVoiceReady = true
	media := &sessionTestMedia{done: make(chan struct{})}
	s, err := a.startRemoteSession(context.Background(), backendFor(a, call).owner.binding, func(remotecall.Binding) bool { return true }, func(context.Context) (remotecall.Media, error) { return media, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Cancel(); waitSession(t, s) })
	sessionCommand(t, s, "answer")
	sessionState(t, s, "awaiting_audio")
	if media.activated.Load() {
		t.Fatal("outgoing audio opened early")
	}
	if err := s.AudioActive("phone-1", call.UUID, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	sessionState(t, s, "connected")
	sessionCommand(t, s, "hangup")
	waitSession(t, s)
	if a.hasActiveCall() || a.callAudioLease.Generation != 0 {
		t.Fatal("outgoing hangup retained call/lease")
	}
}

func TestUnknownOutgoingATResultReconcilesIndexAndDoesNotRedial(t *testing.T) {
	a, call := remoteOutgoing(t)
	a.demo = false
	a.prepareCallVoice = func(context.Context) error { return nil }
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	var trace []string
	dialed := false
	a.callAT = func(command string, _ time.Duration) (string, error) {
		trace = append(trace, command)
		switch command {
		case "ATD12345;":
			dialed = true
			return "", errors.New("unknown AT outcome")
		case "AT+CLCC":
			if dialed {
				return "+CLCC: 7,0,2,0,0,\"12345\",129\nOK", nil
			}
			return "OK", nil
		default:
			return "OK", nil
		}
	}
	attempted, err := b.Answer(context.Background())
	if !attempted || err == nil {
		t.Fatal("unknown dial outcome lost")
	}
	if err := b.Finish(context.Background(), attempted); err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(trace, "\n"), "ATD12345;") != 1 || trace[len(trace)-1] != "ATH" {
		t.Fatal("redialed or unbound teardown", trace)
	}
	a.applyCallPoll(nil, time.Now())
	if a.callAudioLease.Generation != 0 {
		t.Fatal("lease retained after CLCC confirmed teardown")
	}
}
