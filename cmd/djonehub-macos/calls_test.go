package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseCLCCIncomingCall(t *testing.T) {
	got := parseCLCC("AT+CLCC\r\n+CLCC: 1,1,4,0,0,\"13800138000\",129\r\nOK")
	if len(got) != 1 {
		t.Fatalf("parseCLCC() len=%d, want 1", len(got))
	}
	if got[0].Index != 1 || got[0].Direction != "incoming" ||
		got[0].State != "incoming" || got[0].Number != "13800138000" {
		t.Fatalf("parseCLCC()=%+v", got[0])
	}
}

func TestParseCLCCIgnoresDataSession(t *testing.T) {
	response := "AT+CLCC\r\n+CLCC: 2,1,0,1,0,\"\",128\r\nOK"
	if got := parseCLCC(response); len(got) != 0 {
		t.Fatalf("parseCLCC()=%+v, want data session ignored", got)
	}
}

func TestCallLifecycleMarksMissed(t *testing.T) {
	a := &app{
		callPollInterval:   defaultCallPollInterval,
		barkSettingsLoaded: true,
		barkSettings:       normalizeBarkSettings(barkSettings{}),
	}
	started := time.Date(2026, 8, 10, 10, 0, 0, 0, time.Local)
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
	}}, started)
	a.applyCallPoll(nil, started.Add(8*time.Second))

	if a.activeCall != nil {
		t.Fatal("active call was not cleared")
	}
	if len(a.callHistory) != 1 || !a.callHistory[0].Missed {
		t.Fatalf("history=%+v, want one missed call", a.callHistory)
	}
}

func TestAnsweredCallIsNotMissed(t *testing.T) {
	a := &app{
		callPollInterval:   defaultCallPollInterval,
		barkSettingsLoaded: true,
		barkSettings:       normalizeBarkSettings(barkSettings{}),
	}
	started := time.Date(2026, 8, 10, 10, 0, 0, 0, time.Local)
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
	}}, started)
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "active", Number: "10086",
	}}, started.Add(3*time.Second))
	a.applyCallPoll(nil, started.Add(8*time.Second))

	if len(a.callHistory) != 1 || a.callHistory[0].Missed {
		t.Fatalf("history=%+v, want answered call", a.callHistory)
	}
}

func TestValidateCallNumber(t *testing.T) {
	for _, input := range []string{"10086", "+8613800138000", strings.Repeat("8", 32)} {
		if got, err := validateCallNumber(input); err != nil || got != input {
			t.Fatalf("validateCallNumber(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "+", "100 86", "100-86", "*100#", strings.Repeat("8", 33)} {
		if _, err := validateCallNumber(input); err == nil {
			t.Fatalf("validateCallNumber(%q) unexpectedly succeeded", input)
		}
	}
}

func TestDialCallHTTP(t *testing.T) {
	var commands []string
	a := &app{
		demo: true,
		callATRunner: func(command string, _ time.Duration) (string, error) {
			commands = append(commands, command)
			return "OK", nil
		},
	}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/dial", `{"number":"10086"}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(commands) != 1 || commands[0] != "ATD10086;" {
		t.Fatalf("commands=%v", commands)
	}
	if a.activeCall == nil || a.activeCall.Direction != "outgoing" || a.activeCall.State != "dialing" {
		t.Fatalf("activeCall=%+v", a.activeCall)
	}
}

func TestDialCallHTTPRejectsInvalidNumber(t *testing.T) {
	called := false
	a := &app{demo: true, callATRunner: func(string, time.Duration) (string, error) {
		called = true
		return "OK", nil
	}}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/dial", `{"number":"100 86"}`)
	assertCallError(t, response, http.StatusUnprocessableEntity, "invalid_number")
	if called {
		t.Fatal("AT runner called for an invalid number")
	}
}

func TestDialCallHTTPRejectsConcurrentCall(t *testing.T) {
	a := &app{demo: true, activeCall: &callRecord{State: "incoming", Direction: "incoming"}}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/dial", `{"number":"10086"}`)
	assertCallError(t, response, http.StatusConflict, "call_state_conflict")
}

func TestAnswerCallHTTP(t *testing.T) {
	var command string
	a := &app{
		demo:       true,
		activeCall: &callRecord{State: "incoming", Direction: "incoming", Number: "10010"},
		callATRunner: func(value string, _ time.Duration) (string, error) {
			command = value
			return "OK", nil
		},
	}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/answer", "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if command != "ATA" {
		t.Fatalf("command=%q", command)
	}
}

func TestAnswerCallHTTPRequiresIncomingCall(t *testing.T) {
	a := &app{demo: true}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/answer", "")
	assertCallError(t, response, http.StatusConflict, "call_state_conflict")
}

func TestHangupCallHTTPIsIdempotent(t *testing.T) {
	called := false
	a := &app{demo: true, callATRunner: func(string, time.Duration) (string, error) {
		called = true
		return "OK", nil
	}}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/hangup", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if called {
		t.Fatal("idle hangup should not send ATH")
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["noop"] != true {
		t.Fatalf("body=%v", body)
	}
}

func TestHangupCallHTTPFinalizesActiveCall(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	a := &app{
		demo:       true,
		activeCall: &callRecord{ID: "call-1", State: "active", Direction: "outgoing", StartedAt: started},
		callATRunner: func(command string, _ time.Duration) (string, error) {
			if command != "ATH" {
				t.Fatalf("command=%q", command)
			}
			return "OK", nil
		},
		barkSettingsLoaded: true,
		barkSettings:       normalizeBarkSettings(barkSettings{}),
	}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/hangup", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if a.activeCall != nil || len(a.callHistory) != 1 || a.callHistory[0].EndedAt == nil {
		t.Fatalf("active=%+v history=%+v", a.activeCall, a.callHistory)
	}
}

func TestCallHTTPMapsModemAndTransportErrors(t *testing.T) {
	t.Run("modem rejected", func(t *testing.T) {
		a := &app{demo: true, callATRunner: func(string, time.Duration) (string, error) {
			return "+CME ERROR: 3", nil
		}}
		response := performCallRequest(t, a, http.MethodPost, "/api/calls/dial", `{"number":"10086"}`)
		assertCallError(t, response, http.StatusBadGateway, "modem_rejected")
	})
	t.Run("transport unavailable", func(t *testing.T) {
		a := &app{demo: true, callATRunner: func(string, time.Duration) (string, error) {
			return "", errors.New("disconnected")
		}}
		response := performCallRequest(t, a, http.MethodPost, "/api/calls/dial", `{"number":"10086"}`)
		assertCallError(t, response, http.StatusServiceUnavailable, "call_unavailable")
	})
}

func TestCallStatusAdvertisesSignalingWithoutAudio(t *testing.T) {
	a := &app{demo: true, callPollInterval: defaultCallPollInterval}
	response := performCallRequest(t, a, http.MethodGet, "/api/calls/status", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Capabilities struct {
			Signaling bool `json:"signaling"`
			Audio     bool `json:"audio"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Capabilities.Signaling || body.Capabilities.Audio {
		t.Fatalf("capabilities=%+v", body.Capabilities)
	}
}

func performCallRequest(t *testing.T, a *app, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	return response
}

func assertCallError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != code || body["error"] == "" {
		t.Fatalf("body=%v", body)
	}
}
