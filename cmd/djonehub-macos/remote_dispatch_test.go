package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func dispatchApp(t *testing.T) *app {
	t.Helper()
	a := remoteTestApp(t)
	a.deviceID = "dji-test"
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	return a
}

func TestRemoteDispatchAIUnavailableNeverReachesTransport(t *testing.T) {
	a := dispatchApp(t)
	agent := a.ensureVoiceAgent()
	agent.mu.Lock()
	agent.state.Enabled = true
	agent.state.LastError = "provider unavailable"
	agent.mu.Unlock()
	var attempts atomic.Int32
	a.remoteDispatch = func(context.Context, callRecord) { attempts.Add(1) }
	for range 10 {
		a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	}
	if attempts.Load() != 0 {
		t.Fatal("AI failure entered remote dispatch")
	}
}

func TestRemoteDispatchOncePerUUIDAndCancelsReplacedCall(t *testing.T) {
	a := dispatchApp(t)
	var attempts atomic.Int32
	var first context.Context
	var uuid string
	a.remoteDispatch = func(ctx context.Context, c callRecord) {
		if attempts.Add(1) == 1 {
			first, uuid = ctx, c.UUID
		}
	}
	incoming := []parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "123"}}
	a.applyCallPoll(incoming, time.Now())
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); a.dispatchRemoteIncoming() }()
	}
	wg.Wait()
	// Includes a previously rejected/offline attempt: reconnection is not a
	// reason to try the same UUID again on the next poll.
	a.applyCallPoll(incoming, time.Now())
	if attempts.Load() != 1 {
		t.Fatal("duplicate invite", attempts.Load())
	}
	incoming[0].Number = "456"
	a.applyCallPoll(incoming, time.Now())
	if attempts.Load() != 2 || first.Err() == nil || a.activeCall.UUID == uuid {
		t.Fatal("replacement retained old work")
	}
	a.applyCallPoll(nil, time.Now())
}

func TestRemoteDispatchSkipsClaimedExpiredWaitingAndUnscopedCalls(t *testing.T) {
	for _, kind := range []string{"claimed", "expired", "waiting", "unscoped", "outgoing", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			a := dispatchApp(t)
			a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
			a.remoteDispatch = func(context.Context, callRecord) { t.Fatal("ineligible call dispatched") }
			a.callMu.Lock()
			switch kind {
			case "claimed":
				a.activeCall.ControlOwner = "local"
			case "expired":
				a.activeCall.StartedAt = time.Now().Add(-2 * time.Minute)
			case "waiting":
				a.activeCall.State = "waiting"
			case "unscoped":
				a.deviceID = ""
			case "outgoing":
				a.activeCall.Direction = "outgoing"
			case "canceled":
				a.callCancel()
			}
			a.callMu.Unlock()
			a.dispatchRemoteIncoming()
		})
	}
}

func TestRemoteStatusSeparatesOnlineTransportFromProductionAvailability(t *testing.T) {
	a := dispatchApp(t)
	a.remoteConnectionState = func() string { return "online" }
	r := remoteRequest(a, http.MethodGet, "/api/remote-calls/status", "")
	var status struct {
		Available bool
		Reason    string
		Online    struct {
			Configured bool
			State      string
		} `json:"online_transport"`
	}
	if json.Unmarshal(r.Body.Bytes(), &status) != nil || status.Available || status.Reason != "remote_validation_pending" || !status.Online.Configured || status.Online.State != "online" {
		t.Fatal(r.Body.String())
	}
}
