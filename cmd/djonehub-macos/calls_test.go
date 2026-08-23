package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestNormalizeDialNumber(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "+86 138-0013-8000", want: "+8613800138000"},
		{input: "10086", want: "10086"},
		{input: "*100#", want: "*100#"},
		{input: "10086;AT+CFUN=1", want: ""},
		{input: "", want: ""},
	} {
		if got := normalizeDialNumber(test.input); got != test.want {
			t.Fatalf("normalizeDialNumber(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestValidateCallATResponseRejectsModemError(t *testing.T) {
	if err := validateCallATResponse("ATD10086;\r\nERROR"); err == nil {
		t.Fatal("validateCallATResponse accepted modem ERROR")
	}
	if err := validateCallATResponse("ATD10086;\r\nOK"); err != nil {
		t.Fatalf("validateCallATResponse rejected OK: %v", err)
	}
}

func TestDemoCallControlLifecycle(t *testing.T) {
	instance := newDemoApp()
	started := time.Now()
	instance.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
	}}, started)

	answerRequest := httptest.NewRequest(http.MethodPost, "/api/calls/answer", nil)
	answerResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(answerResponse, answerRequest)
	if answerResponse.Code != http.StatusOK {
		t.Fatalf("answer status = %d, body = %s", answerResponse.Code, answerResponse.Body.String())
	}
	if !instance.callStateAllows("active") {
		t.Fatal("demo answer did not move call to active")
	}

	dtmfRequest := httptest.NewRequest(http.MethodPost, "/api/calls/dtmf", bytes.NewBufferString(`{"digit":"5"}`))
	dtmfResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(dtmfResponse, dtmfRequest)
	if dtmfResponse.Code != http.StatusOK {
		t.Fatalf("DTMF status = %d, body = %s", dtmfResponse.Code, dtmfResponse.Body.String())
	}

	hangupRequest := httptest.NewRequest(http.MethodPost, "/api/calls/hangup", nil)
	hangupResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(hangupResponse, hangupRequest)
	if hangupResponse.Code != http.StatusOK {
		t.Fatalf("hangup status = %d, body = %s", hangupResponse.Code, hangupResponse.Body.String())
	}
	if instance.hasActiveCall() {
		t.Fatal("demo hangup left an active call")
	}
}
