package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
)

func TestRemoteCallPollIntervals(t *testing.T) {
	a := &app{demo: true}
	remote := a.ensureRemoteCalls()
	remote.config.Enabled = true
	if got := a.callInterval(); got != time.Second {
		t.Fatalf("remote idle: %v", got)
	}
	agent := a.ensureVoiceAgent()
	agent.state.Enabled = true
	if got := a.callInterval(); got != 3*time.Second {
		t.Fatalf("AI must preserve its poll cadence: %v", got)
	}
	agent.state.Enabled = false
	for _, tc := range []struct {
		state, control string
		want           time.Duration
	}{
		{"incoming", "", 250 * time.Millisecond},
		{"dialing", "dial_pending", 250 * time.Millisecond},
		{"active", "answering", 250 * time.Millisecond},
		{"active", "hangup_pending", 250 * time.Millisecond},
		{"active", "answered", time.Second},
		{"held", "", time.Second},
	} {
		a.activeCall = &callRecord{State: tc.state, ControlState: tc.control, Route: callcontrol.RouteSnapshot{Mode: callcontrol.Remote}}
		if got := a.callInterval(); got != tc.want {
			t.Errorf("%s/%s: %v want %v", tc.state, tc.control, got, tc.want)
		}
	}
	// The frozen call route wins even if idle configuration changes.
	remote.config.Enabled = false
	a.activeCall.Route.Mode = callcontrol.Remote
	if got := a.callInterval(); got != time.Second {
		t.Fatal(got)
	}
	a.activeCall.Route.Mode = callcontrol.Agent
	if got := a.callInterval(); got != 3*time.Second {
		t.Fatal(got)
	}
	a.activeCall = nil
	if got := a.callInterval(); got != 3*time.Second {
		t.Fatal(got)
	}
	remote.config.Enabled = true
	a.callPollInterval = 100 * time.Millisecond
	if got := a.callInterval(); got != 100*time.Millisecond {
		t.Fatal(got)
	}
}

func TestCallStatusReportsFractionalPollInterval(t *testing.T) {
	a := &app{demo: true, activeCall: &callRecord{State: "incoming", Route: callcontrol.RouteSnapshot{Mode: callcontrol.Remote}}}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { a.callStatus(w, httptest.NewRequest("GET", "/", nil)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callStatus deadlocked")
	}
	var body struct {
		Seconds      float64 `json:"poll_interval_s"`
		Milliseconds int     `json:"poll_interval_ms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Seconds != .25 || body.Milliseconds != 250 {
		t.Fatalf("%+v", body)
	}
}
