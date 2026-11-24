package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSMSAppRoutingPausesAndRestoresBarkPerSIM(t *testing.T) {
	a := remoteTestApp(t)
	bark := make(chan string, 8)
	a.barkHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		bark <- r.URL.Path
		return barkSuccessResponse(r), nil
	})}
	channel := barkChannelSettings{Enabled: true, APIURL: "https://example.invalid/{message}", Alias: "主卡"}
	a.barkSettings = normalizeBarkSettings(barkSettings{SMS: channel, MissedCall: channel})
	before := a.barkSettings
	var targets []string
	a.remoteSMSDispatch = func(message receivedSMS, target string) { targets = append(targets, target) }
	configure := func(body string) {
		t.Helper()
		if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", body); r.Code != 200 {
			t.Fatalf("configure: %s", r.Body.String())
		}
	}
	configure(enabledRemoteBody)
	message := receivedSMS{Sender: "10086", Content: "验证码 1234", Timestamp: time.Now()}
	a.mergeSMS([]receivedSMS{message, message})
	a.recordSMS("已发送至 10086", "查询", time.Now())
	a.forwardCallBark(callRecord{Missed: true, Number: "12345"})
	if len(targets) != 1 || targets[0] != "phone-1" {
		t.Fatalf("destinations = %v", targets)
	}
	assertNoBarkRequest(t, bark)
	for _, path := range []string{"/api/settings/bark/sms", "/api/settings/bark/missed-call"} {
		r := remoteRequest(a, http.MethodGet, path, "")
		var status barkChannelStatus
		if err := json.Unmarshal(r.Body.Bytes(), &status); err != nil || !status.PausedByApp || !status.Enabled {
			t.Fatalf("status = %s", r.Body.String())
		}
	}
	// The persisted switch keeps control after restart, including when AI wins calls.
	restarted := remoteTestApp(t)
	restarted.remoteCallSettingsPath = a.remoteCallSettingsPath
	restarted.ensureVoiceAgent().state.Enabled = true
	if restarted.appNotificationTarget() != "phone-1" {
		t.Fatal("notification route did not survive restart")
	}
	if other := remoteTestApp(t); other.appNotificationTarget() != "" {
		t.Fatal("another SIM was paused")
	}
	configure(`{"enabled":true,"target_installation_id":"phone-2"}`)
	a.recordSMS("10010", "新目标", time.Now())
	if len(targets) != 2 || targets[1] != "phone-2" {
		t.Fatal(targets)
	}
	configure(`{"enabled":false}`)
	a.recordSMS("10086", "恢复短信通知", time.Now())
	a.forwardCallBark(callRecord{Missed: true, Number: "12345"})
	for i := 0; i < 2; i++ {
		select {
		case <-bark:
		case <-time.After(time.Second):
			t.Fatal("Bark not restored")
		}
	}
	if a.barkSettings != before || len(targets) != 2 {
		t.Fatal("original Bark settings changed or disabled route forwarded to app")
	}
}

func TestSMSAppRouteDoesNotFallBackToBarkWhileOffline(t *testing.T) {
	a := remoteTestApp(t)
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); r.Code != 200 {
		t.Fatal(r.Code)
	}
	a.barkSettings = normalizeBarkSettings(barkSettings{SMS: barkChannelSettings{Enabled: true, APIURL: "https://example.invalid/{message}"}})
	received := make(chan string, 1)
	a.barkHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		received <- r.URL.Path
		return barkSuccessResponse(r), nil
	})}
	a.recordSMS("10086", "离线短信", time.Now())
	assertNoBarkRequest(t, received)
}
