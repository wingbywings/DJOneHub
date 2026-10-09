package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/voiceagent"
)

func startCommandCall(a *app, state string) callRecord {
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: state, Number: "10086"}}, time.Now())
	call, _ := a.currentCallIdentity()
	return call
}
func commandFor(call callRecord, action string) callcontrol.Command {
	return callcontrol.Command{RequestID: callcontrol.NewCallUUID(), CallUUID: call.UUID, Type: action, Actor: "local", ExpectedRevision: call.Revision}
}
func requireCommandCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *callCommandError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("error=%v, want %s", err, code)
	}
}

func TestBoundAnswerRetryAndRevision(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "incoming")
	command := commandFor(call, "answer")
	var preparations atomic.Int32
	a.prepareCallVoice = func(context.Context) error { preparations.Add(1); return nil }
	first, err := a.executeCallCommand(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.executeCallCommand(context.Background(), command)
	if err != nil || first != second || preparations.Load() != 1 {
		t.Fatalf("retry=%+v, %v count=%d", second, err, preparations.Load())
	}
	stale := commandFor(call, "hangup")
	_, err = a.executeCallCommand(context.Background(), stale)
	requireCommandCode(t, err, "stale_revision")
}

func TestAnswerPreparationCannotCommitAfterReplacement(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "incoming")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	a.prepareCallVoice = func(context.Context) error { close(entered); <-release; return nil }
	go func() { _, err := a.executeCallCommand(context.Background(), commandFor(call, "answer")); done <- err }()
	<-entered
	a.applyCallPoll(nil, time.Now())
	next := startCommandCall(a, "incoming")
	nextLease, err := a.reserveCallAudio(next.UUID)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("late answer=%v", err)
	}
	current, _ := a.currentCallIdentity()
	if current.UUID != next.UUID || current.State != "incoming" {
		t.Fatalf("new call changed: %+v", current)
	}
	if a.callAudioLease != nextLease {
		t.Fatal("old completion released new lease")
	}
	_, err = a.executeCallCommand(context.Background(), commandFor(call, "hangup"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.callMatches(next.UUID, "incoming") {
		t.Fatal("old hangup ended replacement")
	}
}

func TestHangupCancelsPendingAnswer(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "incoming")
	entered, done := make(chan struct{}), make(chan error, 1)
	a.prepareCallVoice = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	go func() { _, err := a.executeCallCommand(context.Background(), commandFor(call, "answer")); done <- err }()
	<-entered
	current, _ := a.currentCallIdentity()
	if _, err := a.executeCallCommand(context.Background(), commandFor(current, "hangup")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a.hasActiveCall() {
		t.Fatal("hangup did not end call")
	}
	if a.callAudioLease.Generation != 0 {
		t.Fatal("cancelled answer retained audio")
	}
}

func TestAnswerAudioOwnershipBeforePreparation(t *testing.T) {
	first, second := remoteTestApp(t), remoteTestApp(t)
	hub := &usbDeviceHub{}
	hub.configureVoiceRouteGuard(first, "one")
	hub.configureVoiceRouteGuard(second, "two")
	call1, call2 := startCommandCall(first, "incoming"), startCommandCall(second, "incoming")
	if _, err := first.reserveCallAudio(call1.UUID); err != nil {
		t.Fatal(err)
	}
	second.prepareCallVoice = func(context.Context) error { t.Fatal("busy call prepared audio"); return nil }
	_, err := second.executeCallCommand(context.Background(), commandFor(call2, "answer"))
	if !errors.Is(err, callcontrol.ErrAudioBusy) {
		t.Fatalf("busy=%v", err)
	}
	if !second.callMatches(call2.UUID, "incoming") {
		t.Fatal("busy call was answered")
	}
	first.applyCallPoll(nil, time.Now())
	second.prepareCallVoice = func(context.Context) error { return nil }
	current, _ := second.currentCallIdentity()
	if _, err := second.executeCallCommand(context.Background(), commandFor(current, "answer")); err != nil {
		t.Fatal(err)
	}
}

func TestBoundATRejectsAmbiguousOrChangedCalls(t *testing.T) {
	for _, response := range []string{
		"+CLCC: 1,1,4,0,0,\"10086\",129\r\n+CLCC: 2,1,5,0,0,\"10010\",129\r\nOK",
		"+CLCC: 1,1,4,0,0,\"10010\",129\r\nOK",
	} {
		t.Run(response, func(t *testing.T) {
			a := remoteTestApp(t)
			call := startCommandCall(a, "incoming")
			a.demo = false
			a.prepareCallVoice = func(context.Context) error { return nil }
			var commands []string
			a.callAT = func(cmd string, _ time.Duration) (string, error) {
				commands = append(commands, cmd)
				return response, nil
			}
			if _, err := a.executeCallCommand(context.Background(), commandFor(call, "answer")); err == nil {
				t.Fatal("unsafe answer succeeded")
			}
			if !reflect.DeepEqual(commands, []string{"AT+CLCC"}) {
				t.Fatalf("commands=%v", commands)
			}
		})
	}
}

func TestDTMFTransportTimeoutDoesNotSendFallback(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "active")
	a.demo = false
	var commands []string
	a.callAT = func(cmd string, _ time.Duration) (string, error) {
		commands = append(commands, cmd)
		if cmd == "AT+CLCC" {
			return "+CLCC: 1,1,0,0,0,\"10086\",129\r\nOK", nil
		}
		return "", context.DeadlineExceeded
	}
	command := commandFor(call, "dtmf")
	command.Digit = "5"
	if _, err := a.executeCallCommand(context.Background(), command); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	_, _ = a.executeCallCommand(context.Background(), command)
	if !reflect.DeepEqual(commands, []string{"AT+CLCC", "AT+VTS=\"5\""}) {
		t.Fatalf("duplicated digit: %v", commands)
	}
}

func TestDelayedAIHangupDoesNotTargetNextCall(t *testing.T) {
	a := remoteTestApp(t)
	controller := a.ensureVoiceAgent()
	controller.state.Enabled = true
	call := startCommandCall(a, "active")
	controller.runtime.pendingHangups[call.ID] = &pendingVoiceAgentHangup{CallID: call.ID, CallUUID: call.UUID, Armed: true, ToolCall: &voiceagent.ToolCall{ID: "tool", Name: "hang_up_call"}}
	a.applyCallPoll(nil, time.Now())
	next := startCommandCall(a, "active")
	// Reintroduce a completion which was already dequeued during teardown.
	controller.runtime.pendingHangups[call.ID] = &pendingVoiceAgentHangup{CallID: call.ID, CallUUID: call.UUID, Armed: true, ToolCall: &voiceagent.ToolCall{ID: "tool", Name: "hang_up_call"}}
	a.completeVoiceAgentHangup(controller, call.ID)
	if !a.callMatches(next.UUID, "active") {
		t.Fatal("old AI callback hung up new call")
	}
}

func TestCallCommandAPIRequiresRevisionAndBindsActor(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "incoming")
	path := "/api/calls/" + call.UUID + "/commands"
	response := remoteRequest(a, http.MethodPost, path, `{"request_id":"r","type":"answer"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
	command := commandFor(call, "answer")
	body, _ := json.Marshal(command)
	response = remoteRequest(a, http.MethodPost, path, string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCLCCReplacementGetsNewIdentityWithoutEmptyPoll(t *testing.T) {
	a := remoteTestApp(t)
	previous := startCommandCall(a, "active")
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10010"}}, time.Now())
	next, _ := a.currentCallIdentity()
	if next.UUID == previous.UUID {
		t.Fatal("replacement inherited UUID")
	}
	if _, err := a.executeCallCommand(context.Background(), commandFor(previous, "hangup")); err != nil {
		t.Fatal(err)
	}
	if !a.callMatches(next.UUID, "incoming") {
		t.Fatal("old command changed replacement")
	}
}

func TestCancelledQueuedCommandNeverWritesAT(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "active")
	a.demo = false
	a.callAT = func(string, time.Duration) (string, error) { t.Error("cancelled command reached AT"); return "OK", nil }
	a.operationMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.executeCallCommand(ctx, commandFor(call, "hangup"))
	a.operationMu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	current, _ := a.currentCallIdentity()
	if current.ControlState != "" {
		t.Fatalf("cancelled command locked call: %+v", current)
	}
}

func TestExplicitAnswerRefusalReleasesLeaseButUnknownResultRetainsIt(t *testing.T) {
	for _, response := range []string{"ERROR", ""} {
		t.Run(response, func(t *testing.T) {
			a := remoteTestApp(t)
			call := startCommandCall(a, "incoming")
			a.demo = false
			a.prepareCallVoice = func(context.Context) error { return nil }
			a.callAT = func(cmd string, _ time.Duration) (string, error) {
				if cmd == "AT+CLCC" {
					return "+CLCC: 1,1,4,0,0,\"10086\",129\r\nOK", nil
				}
				return response, nil
			}
			_, err := a.executeCallCommand(context.Background(), commandFor(call, "answer"))
			if err == nil {
				t.Fatal("unconfirmed answer succeeded")
			}
			current, _ := a.currentCallIdentity()
			if response == "ERROR" {
				if a.callAudioLease.Generation != 0 || current.ControlState != "" {
					t.Fatal("explicit refusal kept lease")
				}
			} else {
				if a.callAudioLease.Generation == 0 || current.ControlState != "answer_pending" {
					t.Fatal("unknown ATA result released ownership")
				}
			}
		})
	}
}

func TestOldAnswerCleanupCannotClearNewAttempt(t *testing.T) {
	a := remoteTestApp(t)
	call := startCommandCall(a, "incoming")
	a.demo = false
	entered1, release1, entered2, release2 := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	a.prepareCallVoice = func(context.Context) error {
		if n.Add(1) == 1 {
			close(entered1)
			<-release1
		} else {
			close(entered2)
			<-release2
		}
		return nil
	}
	a.callAT = func(string, time.Duration) (string, error) { return "OK", nil } // No CLCC call: reject hangup before write.
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { _, err := a.executeCallCommand(context.Background(), commandFor(call, "answer")); done1 <- err }()
	<-entered1
	current, _ := a.currentCallIdentity()
	_, err := a.executeCallCommand(context.Background(), commandFor(current, "hangup"))
	requireCommandCode(t, err, "ambiguous_call")
	current, _ = a.currentCallIdentity()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := a.executeCallCommand(ctx, commandFor(current, "answer")); done2 <- err }()
	<-entered2
	close(release1)
	if err := <-done1; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, _ := a.currentCallIdentity()
	if after.ControlState != "answering" || a.callAudioLease.Generation == 0 {
		t.Fatal("old cleanup corrupted new answer")
	}
	cancel()
	close(release2)
	<-done2
}
