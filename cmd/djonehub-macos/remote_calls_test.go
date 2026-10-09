package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/voiceagent"
)

func remoteTestApp(t *testing.T) *app {
	t.Helper()
	a := &app{demo: true, barkSettingsLoaded: true}
	agent := newVoiceAgentController("")
	agent.state = voiceAgentState{Provider: "qwen", Revision: 1}
	agent.keys = map[string]bool{"qwen": true}
	a.voiceAgentOnce.Do(func() { a.voiceAgent = agent })
	a.remoteCallSettingsPath = filepath.Join(t.TempDir(), "remote-calls.json")
	return a
}

func remoteRequest(a *app, method, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	return response
}

const enabledRemoteBody = `{"enabled":true,"target_installation_id":"phone-1"}`

func TestRemoteConfigPersistsAtomicallyWithoutAISettings(t *testing.T) {
	a := remoteTestApp(t)
	response := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody)
	if response.Code != http.StatusOK {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	config, loadError := newRemoteCallController(a.remoteCallSettingsPath).snapshot()
	if loadError != "" || !config.Enabled || config.TargetInstallationID != "phone-1" || config.Revision != 2 || config.RingTimeoutSeconds != 45 {
		t.Fatalf("stored config=%#v error=%s", config, loadError)
	}
	info, err := os.Stat(a.remoteCallSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if a.ensureVoiceAgent().snapshot().Enabled {
		t.Fatal("remote configuration enabled AI")
	}
	response = remoteRequest(a, http.MethodGet, "/api/remote-calls/status", "")
	var status struct {
		Available      bool
		Reason         string
		EffectiveRoute string `json:"effective_route"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Available || status.Reason != "transport_not_implemented" || status.EffectiveRoute != "remote" {
		t.Fatalf("status=%s", response.Body.String())
	}
}

func TestRemoteConfigFailedSaveDoesNotPublish(t *testing.T) {
	a := remoteTestApp(t)
	controller := a.ensureRemoteCalls()
	before, _ := controller.snapshot()
	if err := os.Mkdir(a.remoteCallSettingsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	response := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	after, _ := controller.snapshot()
	if after != before {
		t.Fatalf("failed save changed state: %#v", after)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(a.remoteCallSettingsPath), ".remote-calls-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files=%v error=%v", matches, err)
	}
}

func TestCorruptRemoteConfigIsDisabledAndCanBeRepaired(t *testing.T) {
	a := remoteTestApp(t)
	if err := os.WriteFile(a.remoteCallSettingsPath, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, loadError := a.ensureRemoteCalls().snapshot()
	if config.Enabled || loadError == "" {
		t.Fatalf("config=%#v error=%s", config, loadError)
	}
	response := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("repair: %s", response.Body.String())
	}
	_, loadError = newRemoteCallController(a.remoteCallSettingsPath).snapshot()
	if loadError != "" {
		t.Fatalf("repair failed: %s", loadError)
	}
}

func TestCallRouteAndUUIDSurviveLifecycle(t *testing.T) {
	a := remoteTestApp(t)
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	now := time.Now()
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, now)
	first := *a.activeCall
	if first.UUID == "" || first.Route.Mode != callcontrol.Remote || first.Route.TargetInstallationID != "phone-1" {
		t.Fatalf("call=%#v", first)
	}
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "active"}}, now.Add(time.Second))
	if a.activeCall.UUID != first.UUID || a.activeCall.Route != first.Route || a.activeCall.AIHandled || a.audioHost.WantRecording {
		t.Fatalf("changed route or AI recording: %#v", a.activeCall)
	}
	a.applyCallPoll(nil, now.Add(2*time.Second))
	if a.callHistory[0].UUID != first.UUID || a.callHistory[0].Route != first.Route {
		t.Fatal("archive lost call identity")
	}
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, now.Add(3*time.Second))
	if a.activeCall.UUID == first.UUID {
		t.Fatal("reused modem index reused UUID")
	}
}

func TestAIAlwaysSuppressesRemoteRoute(t *testing.T) {
	a := remoteTestApp(t)
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	agent := a.ensureVoiceAgent()
	agent.mu.Lock()
	agent.state.Enabled = true
	agent.state.LastError = "provider unavailable"
	agent.mu.Unlock()
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	if a.activeCall.Route.Mode != callcontrol.Agent || a.activeCall.Route.TargetInstallationID != "" {
		t.Fatalf("AI leaked remote route: %#v", a.activeCall.Route)
	}
	r := remoteRequest(a, http.MethodGet, "/api/remote-calls/status", "")
	if !bytes.Contains(r.Body.Bytes(), []byte(`"reason":"ai_route_selected"`)) {
		t.Fatal(r.Body.String())
	}
}

func TestRemoteCallCannotEnterAIOrRecordEvenWithStaleEnabledProfile(t *testing.T) {
	a := remoteTestApp(t)
	a.demo = false // Exercise the real preparation gate without any hardware.
	agent := a.ensureVoiceAgent()
	agent.state.Enabled, agent.state.AutoAnswer, agent.state.AutoAnswerDelayMS = true, true, 1
	opened := make(chan struct{}, 1)
	agent.openSessionOverride = func(context.Context, voiceAgentState, voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
		opened <- struct{}{}
		return &trackedVoiceAgentSession{closed: make(chan struct{})}, voiceAgentState{}, nil
	}
	a.activeCall = &callRecord{ID: "remote", State: "incoming", Direction: "incoming", Route: callcontrol.RouteSnapshot{Mode: callcontrol.Remote}}
	a.syncVoiceAgentPreparation()
	a.scheduleVoiceAgentAutoAnswer("", "incoming")
	time.Sleep(30 * time.Millisecond)
	select {
	case <-opened:
		t.Fatal("remote call opened an AI provider")
	default:
	}
	if !a.lastAnswerAt.IsZero() {
		t.Fatal("remote call attempted AI auto-answer")
	}
	a.setActiveCallState("active", time.Now())
	if a.activeCall.AIHandled || a.audioHost.WantRecording {
		t.Fatal("remote call started AI recording")
	}
	r := remoteRequest(a, http.MethodGet, "/api/calls/audio/agent/media?token="+agent.mediaToken, "")
	if r.Code != http.StatusConflict {
		t.Fatalf("AI media route allowed remote call: %d", r.Code)
	}
	a.usbLocator = &usbDeviceLocator{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, LocationID: 1}
	r = remoteRequest(a, http.MethodGet, "/api/calls/audio/host/config", "")
	if r.Code != http.StatusOK || bytes.Contains(r.Body.Bytes(), []byte(`"agent_media_url"`)) {
		t.Fatalf("host exposed AI bridge: %s", r.Body.String())
	}
}

func TestRouteConfigurationLockedDuringCall(t *testing.T) {
	a := remoteTestApp(t)
	remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody)
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	for path, body := range map[string]string{
		"/api/remote-calls/config": `{"enabled":false}`,
		"/api/voice-agent/config":  `{"enabled":true,"provider":"qwen"}`,
	} {
		r := remoteRequest(a, http.MethodPut, path, body)
		if r.Code != http.StatusConflict || !bytes.Contains(r.Body.Bytes(), []byte("route_locked_by_call")) {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
	}
	r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody)
	if r.Code != http.StatusOK {
		t.Fatalf("idempotent config rejected: %s", r.Body.String())
	}
	a.applyCallPoll(nil, time.Now())
	r = remoteRequest(a, http.MethodPut, "/api/voice-agent/config", `{"enabled":true,"provider":"qwen"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("idle switch rejected: %s", r.Body.String())
	}
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	r = remoteRequest(a, http.MethodPut, "/api/voice-agent/config", `{"enabled":false,"provider":"qwen"}`)
	if r.Code != http.StatusConflict {
		t.Fatalf("active AI disable allowed: %s", r.Body.String())
	}
}

func TestRemoteConfigurationIsIsolatedByModule(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	first, second := remoteTestApp(t), remoteTestApp(t)
	hub.devices["one"] = &managedUSBDevice{ID: "one", State: "ready", Runtime: first}
	hub.devices["two"] = &managedUSBDevice{ID: "two", State: "ready", Runtime: second}
	r := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(r, httptest.NewRequest(http.MethodPut, "/api/devices/one/remote-calls/config", bytes.NewBufferString(enabledRemoteBody)))
	if r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	c1, _ := first.ensureRemoteCalls().snapshot()
	c2, _ := second.ensureRemoteCalls().snapshot()
	if !c1.Enabled || c2.Enabled {
		t.Fatalf("module isolation: %#v %#v", c1, c2)
	}
}

func TestConcurrentRouteConfigurationAndIncomingCall(t *testing.T) {
	for i := 0; i < 20; i++ {
		a := remoteTestApp(t)
		var wg sync.WaitGroup
		wg.Add(2)
		var response *httptest.ResponseRecorder
		go func() {
			defer wg.Done()
			response = remoteRequest(a, http.MethodPut, "/api/voice-agent/config", `{"enabled":true,"provider":"qwen"}`)
		}()
		go func() {
			defer wg.Done()
			a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
		}()
		wg.Wait()
		mode := a.activeCall.Route.Mode
		if response.Code == http.StatusOK && mode != callcontrol.Agent {
			t.Fatalf("enabled AI with %s call", mode)
		}
		if response.Code == http.StatusConflict && mode != callcontrol.Local {
			t.Fatalf("blocked enable with %s call", mode)
		}
		if response.Code != http.StatusOK && response.Code != http.StatusConflict {
			t.Fatal(response.Body.String())
		}
	}
}

func TestRemoteAnsweringDoesNotCaptureLocalOutgoingCalls(t *testing.T) {
	a := remoteTestApp(t)
	remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody)
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "outgoing", State: "dialing"}}, time.Now())
	if a.activeCall.Route.Mode != callcontrol.Local {
		t.Fatalf("outgoing route=%#v", a.activeCall.Route)
	}
}
