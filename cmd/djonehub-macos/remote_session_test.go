package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/remotecall"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

type sessionTestMedia struct {
	mu        sync.Mutex
	ready     remotemedia.Readiness
	done      chan struct{}
	once      sync.Once
	activated atomic.Bool
}

func (m *sessionTestMedia) TransportConnected() bool { return true }
func (m *sessionTestMedia) UpdateReadiness(state remotemedia.Readiness) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = state
	return nil
}
func (m *sessionTestMedia) AnswerReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready.CellularActive && m.ready.ModuleReady
}
func (m *sessionTestMedia) Activate() error       { m.activated.Store(true); return nil }
func (m *sessionTestMedia) Done() <-chan struct{} { return m.done }
func (m *sessionTestMedia) Close()                { m.once.Do(func() { close(m.done) }) }
func remoteIncoming(t *testing.T) (*app, callRecord) {
	t.Helper()
	a := remoteTestApp(t)
	response := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	return a, startCommandCall(a, "incoming")
}
func backendFor(a *app, call callRecord) *remoteCallBackend {
	return &remoteCallBackend{owner: &remoteCallOwner{app: a, binding: remotecall.Binding{CallUUID: call.UUID, Generation: 1, InstallationID: "phone-1"}}}
}
func waitSession(t *testing.T, s *remotecall.Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session cleanup stalled")
	}
}
func sessionState(t *testing.T, s *remotecall.Session, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.Snapshot().State != state {
		if time.Now().After(deadline) {
			t.Fatalf("want %s got %+v", state, s.Snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}
func sessionCommand(t *testing.T, s *remotecall.Session, kind string) {
	t.Helper()
	snapshot := s.Snapshot()
	_, err := s.Command(context.Background(), "phone-1", remotecall.Request{CallUUID: snapshot.CallUUID, Generation: snapshot.Generation, RequestID: callcontrol.NewCallUUID(), ExpectedRevision: snapshot.Revision, Type: kind})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRemoteOwnerCannotBeForgedOrClaimAIRoute(t *testing.T) {
	a, call := remoteIncoming(t)
	command := commandFor(call, "answer")
	command.Actor = "remote"
	_, err := a.executeCallCommand(context.Background(), command)
	requireCommandCode(t, err, "remote_unauthorized")
	b := backendFor(a, call)
	a.callMu.Lock()
	a.activeCall.Route.Mode = callcontrol.Agent
	a.callMu.Unlock()
	if err := b.Claim(context.Background()); !errors.Is(err, remotecall.ErrRoute) {
		t.Fatal(err)
	}
	if a.callAudioLease.Generation != 0 {
		t.Fatal("AI route claimed remote audio")
	}
	_, err = a.startRemoteSession(context.Background(), b.owner.binding, nil, func(context.Context) (remotecall.Media, error) { t.Fatal("AI route created peer"); return nil, nil })
	if !errors.Is(err, remotecall.ErrRoute) {
		t.Fatal(err)
	}
}

func TestRemoteStatusRetainsEndedSessionReason(t *testing.T) {
	a, call := remoteIncoming(t)
	m := &sessionTestMedia{done: make(chan struct{})}
	binding := remotecall.Binding{CallUUID: call.UUID, Generation: 1, InstallationID: "phone-1"}
	s, err := a.startRemoteSession(context.Background(), binding, nil, func(context.Context) (remotecall.Media, error) { return m, nil })
	if err != nil {
		t.Fatal(err)
	}
	s.Cancel()
	waitSession(t, s)
	response := remoteRequest(a, http.MethodGet, "/api/remote-calls/status", "")
	var status struct {
		Session struct {
			CallUUID  string `json:"call_uuid"`
			State     string `json:"state"`
			EndReason string `json:"end_reason"`
		} `json:"session"`
	}
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Session.CallUUID != call.UUID || status.Session.State != "ended" || status.Session.EndReason != "cancelled" {
		t.Fatal("session outcome absent", response.Body.String())
	}
}
func TestRemoteClaimBlocksLocalAnswerAndReleasesBeforeATA(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := a.currentCallIdentity()
	_, err := a.executeCallCommand(context.Background(), commandFor(current, "answer"))
	requireCommandCode(t, err, "answer_in_progress")
	if err := b.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	current, _ = a.currentCallIdentity()
	if current.ControlOwner != "" || a.callAudioLease.Generation != 0 {
		t.Fatal("pre-ATA claim retained ownership")
	}
	if _, err := a.executeCallCommand(context.Background(), commandFor(current, "answer")); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteSessionUsesBoundCommandsAndAudioActivation(t *testing.T) {
	a, call := remoteIncoming(t)
	a.moduleVoiceReady = true
	m := &sessionTestMedia{done: make(chan struct{})}
	binding := backendFor(a, call).owner.binding
	s, err := a.startRemoteSession(context.Background(), binding, func(remotecall.Binding) bool { return true }, func(context.Context) (remotecall.Media, error) { return m, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Cancel(); waitSession(t, s) })
	sessionCommand(t, s, "answer")
	sessionState(t, s, "awaiting_audio")
	current, _ := a.currentCallIdentity()
	if current.State != "active" || current.ControlOwner != "remote" || m.activated.Load() {
		t.Fatal("incorrect answer readiness", current)
	}
	if err := s.AudioActive("phone-1", call.UUID, binding.Generation, 1, true); err != nil {
		t.Fatal(err)
	}
	sessionState(t, s, "connected")
	sessionCommand(t, s, "hangup")
	waitSession(t, s)
	if a.hasActiveCall() || a.callAudioLease.Generation != 0 || !m.activated.Load() {
		t.Fatal("remote hangup did not finish bound call")
	}
}
func TestRemoteLocalWinnerClosesInvitationWithoutHangup(t *testing.T) {
	a, call := remoteIncoming(t)
	m := &sessionTestMedia{done: make(chan struct{})}
	s, err := a.startRemoteSession(context.Background(), backendFor(a, call).owner.binding, nil, func(context.Context) (remotecall.Media, error) { return m, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.executeCallCommand(context.Background(), commandFor(call, "answer")); err != nil {
		t.Fatal(err)
	}
	waitSession(t, s)
	if !a.callMatches(call.UUID, "active") || s.Snapshot().EndReason != "answered_elsewhere" {
		t.Fatal("local winner was ended", s.Snapshot())
	}
}
func TestRemoteUnknownATATeardownRetainsLeaseUntilCLCC(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.demo = false
	a.prepareCallVoice = func(context.Context) error { return nil }
	var commands []string
	clcc := "+CLCC: 1,1,4,0,0,\"10086\",129\r\nOK"
	a.callAT = func(command string, _ time.Duration) (string, error) {
		commands = append(commands, command)
		switch command {
		case "AT+CLCC":
			return clcc, nil
		case "ATA":
			clcc = "+CLCC: 1,1,0,0,0,\"10086\",129\r\nOK"
			return "", context.DeadlineExceeded
		default:
			return "OK", nil
		}
	}
	mayHaveAnswered, err := b.Answer(context.Background())
	if !mayHaveAnswered || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(mayHaveAnswered, err)
	}
	if err := b.Finish(context.Background(), mayHaveAnswered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(commands, []string{"AT+CLCC", "ATA", "AT+CLCC", "ATH"}) {
		t.Fatal(commands)
	}
	if a.callAudioLease.Generation == 0 {
		t.Fatal("released without confirmed cellular teardown")
	}
	a.applyCallPoll(nil, time.Now())
	if a.callAudioLease.Generation != 0 {
		t.Fatal("confirmed end retained lease")
	}
}
func TestRemoteOldCleanupCannotHangupReplacement(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.applyCallPoll(nil, time.Now())
	next := startCommandCall(a, "incoming")
	lease, err := a.reserveCallAudio(next.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !a.callMatches(next.UUID, "incoming") || a.callAudioLease != lease {
		t.Fatal("old cleanup affected replacement")
	}
}
func TestRemoteReadinessDoesNotTrustLegacyHostRegistration(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.moduleVoiceReady = true
	a.audioHost = audioHostState{Registered: true, Running: true, LastSeen: time.Now()}
	state, err := b.Observe()
	if err != nil || state.ModuleReady {
		t.Fatal("legacy host status authorized remote media", state, err)
	}
}
func TestRemoteHostConfigFailsClosed(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.usbLocator = &usbDeviceLocator{VendorID: 1, ProductID: 2, LocationID: 3}
	response := remoteRequest(a, http.MethodGet, "/api/calls/audio/host/config", "")
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	var config audioHostConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if config.MediaMode != "remote" || config.AgentMediaURL != "" || config.Recording {
		t.Fatal(config)
	}
}

func TestRemoteAndLocalAnswerHaveExactlyOneWinner(t *testing.T) {
	for i := 0; i < 20; i++ {
		a, call := remoteIncoming(t)
		b := backendFor(a, call)
		start := make(chan struct{})
		remoteResult, localResult := make(chan error, 1), make(chan error, 1)
		go func() { <-start; remoteResult <- b.Claim(context.Background()) }()
		go func() {
			<-start
			_, err := a.executeCallCommand(context.Background(), commandFor(call, "answer"))
			localResult <- err
		}()
		close(start)
		remoteErr, localErr := <-remoteResult, <-localResult
		if (remoteErr == nil) == (localErr == nil) {
			t.Fatalf("remote=%v local=%v", remoteErr, localErr)
		}
		if remoteErr == nil {
			if !a.callMatches(call.UUID, "incoming") {
				t.Fatal("losing local answer sent ATA")
			}
			if err := b.Finish(context.Background(), false); err != nil {
				t.Fatal(err)
			}
		} else if !a.callMatches(call.UUID, "active") {
			t.Fatal("local winner was not answered")
		}
	}
}

func TestRemoteCleanupFailureRetainsOwnership(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.demo = false
	a.callAT = func(string, time.Duration) (string, error) { return "", context.DeadlineExceeded }
	if err := b.Finish(context.Background(), true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if a.remoteCallOwner != b.owner || a.callAudioLease.Generation == 0 {
		t.Fatal("unknown cellular state released remote owner")
	}
}

func TestCancelledATAIsNotClassifiedAsAttempted(t *testing.T) {
	a := remoteTestApp(t)
	a.callAT = func(string, time.Duration) (string, error) {
		t.Fatal("cancelled command reached transport")
		return "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, attempted, err := a.sendCallATTracked(ctx, "ATA")
	if attempted || !errors.Is(err, context.Canceled) {
		t.Fatal(attempted, err)
	}
}

func TestRemoteDTMFUsesBoundHardwareCommand(t *testing.T) {
	a, call := remoteIncoming(t)
	b := backendFor(a, call)
	if err := b.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.demo = false
	var commands []string
	a.callAT = func(command string, _ time.Duration) (string, error) {
		commands = append(commands, command)
		if command == "AT+CLCC" {
			return "+CLCC: 1,1,0,0,0,\"10086\",129\r\nOK", nil
		}
		return "OK", nil
	}
	if err := b.DTMF(context.Background(), "#"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(commands, []string{"AT+CLCC", "AT+VTS=\"#\""}) {
		t.Fatal(commands)
	}
	// A stale owner cannot send into a replacement call, even with the same index.
	a.applyCallPoll(nil, time.Now())
	startCommandCall(a, "active")
	if err := b.DTMF(context.Background(), "1"); err == nil {
		t.Fatal("old owner targeted replacement")
	}
	if len(commands) != 2 {
		t.Fatal("replacement received AT command", commands)
	}
}
