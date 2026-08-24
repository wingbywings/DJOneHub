package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestValidateBarkChannelsIndependently(t *testing.T) {
	if err := validateBarkChannelSettings(barkChannelSMS, barkChannelSettings{Enabled: true}); err == nil {
		t.Fatal("enabled SMS Bark settings without an API URL should fail validation")
	}
	if err := validateBarkChannelSettings(barkChannelMissedCall, barkChannelSettings{
		Enabled: true, APIURL: "https://api.day.app/key/{message}", MessageTemplate: "{content}",
	}); err == nil {
		t.Fatal("missed-call template using an SMS variable should fail validation")
	}
	if err := validateBarkChannelSettings(barkChannelMissedCall, barkChannelSettings{
		Enabled: true, APIURL: "https://api.day.app/key/{message}", MessageTemplate: "未接来电 {number}",
	}); err != nil {
		t.Fatalf("valid missed-call settings failed validation: %v", err)
	}
	if err := validateBarkChannelSettings(barkChannelSMS, barkChannelSettings{}); err != nil {
		t.Fatalf("empty disabled SMS settings failed validation: %v", err)
	}
}

func TestRenderBarkTemplatesUseChannelSpecificVariables(t *testing.T) {
	sms := barkChannelSettings{Alias: "短信卡", MessageTemplate: "{alias_prefix}{sender}|{content}|{code}|{timestamp}"}
	smsMessage := renderSMSBarkMessage(sms, receivedSMS{
		Sender: "10086", Content: "验证码 4321", Code: "4321",
		Timestamp: time.Date(2026, 8, 10, 12, 30, 0, 0, time.Local),
	})
	if smsMessage != "[短信卡]10086|验证码 4321|4321|2026-08-10 12:30:00" {
		t.Fatalf("renderSMSBarkMessage() = %q", smsMessage)
	}

	ended := time.Date(2026, 8, 10, 13, 0, 8, 0, time.Local)
	call := barkChannelSettings{Alias: "电话卡", MessageTemplate: "{alias_prefix}{status}|{number}|{started_at}|{duration}"}
	callMessage := renderCallBarkMessage(call, callRecord{
		Number: "13800138000", StartedAt: ended.Add(-8 * time.Second), EndedAt: &ended, Missed: true,
	})
	if callMessage != "[电话卡]未接来电|13800138000|2026-08-10 13:00:00|8秒" {
		t.Fatalf("renderCallBarkMessage() = %q", callMessage)
	}
}

func TestSendBarkNotificationEncodesMessageAndPreservesGroup(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/[短信卡]验证码 1234/5" {
			t.Errorf("path = %q, want decoded Bark message", r.URL.Path)
		}
		if got := r.URL.Query().Get("group"); got != "短信通知" {
			t.Errorf("group = %q, want 短信通知", got)
		}
		return barkSuccessResponse(r), nil
	})}
	settings := barkChannelSettings{
		Enabled: true, APIURL: "https://example.invalid/{message}?group=短信通知",
	}
	if err := sendBarkNotification(context.Background(), client, settings, "[短信卡]验证码 1234/5"); err != nil {
		t.Fatalf("sendBarkNotification() error = %v", err)
	}
}

func TestBarkSettingsAPIPersistsChannelsIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bark-settings.json")
	instance := &app{barkSettingsPath: path}

	smsBody := `{"enabled":true,"api_url":"https://api.day.app/sms/{message}","alias":"短信卡","message_template":"{sender}: {content}"}`
	putBarkSettings(t, instance, "/api/settings/bark/sms", smsBody)
	callBody := `{"enabled":true,"api_url":"https://api.day.app/call/{message}","alias":"电话卡","message_template":"未接来电 {number}"}`
	putBarkSettings(t, instance, "/api/settings/bark/missed-call", callBody)

	if info, err := os.Stat(path); err != nil {
		t.Fatalf("saved Bark config stat error = %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("saved Bark config mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := &app{barkSettingsPath: path}
	sms := getBarkSettings(t, reloaded, "/api/settings/bark/sms")
	call := getBarkSettings(t, reloaded, "/api/settings/bark/missed-call")
	if sms.APIURL != "https://api.day.app/sms/{message}" || sms.MessageTemplate != "{sender}: {content}" {
		t.Fatalf("saved SMS settings = %#v", sms)
	}
	if call.APIURL != "https://api.day.app/call/{message}" || call.MessageTemplate != "未接来电 {number}" {
		t.Fatalf("saved missed-call settings = %#v", call)
	}
}

func TestLegacyBarkSettingsMigrateToSMSOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bark-settings.json")
	legacy := `{"enabled":true,"api_url":"https://api.day.app/legacy/{message}","alias":"旧卡"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	instance := &app{barkSettingsPath: path}
	settings, err := instance.currentBarkSettings()
	if err != nil {
		t.Fatalf("currentBarkSettings() error = %v", err)
	}
	if !settings.SMS.Enabled || settings.SMS.Alias != "旧卡" || settings.SMS.MessageTemplate != defaultSMSBarkTemplate {
		t.Fatalf("migrated SMS settings = %#v", settings.SMS)
	}
	if settings.MissedCall.Enabled || settings.MissedCall.APIURL != "" {
		t.Fatalf("migrated missed-call settings = %#v, want disabled", settings.MissedCall)
	}
}

func TestMergeSMSForwardsOnlyNewIncomingMessages(t *testing.T) {
	received := make(chan string, 3)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		received <- r.URL.Path
		return barkSuccessResponse(r), nil
	})}
	instance := &app{
		barkSettingsLoaded: true,
		barkSettings: normalizeBarkSettings(barkSettings{SMS: barkChannelSettings{
			Enabled: true, APIURL: "https://example.invalid/{message}",
			Alias: "主卡", MessageTemplate: "{alias_prefix}{content}",
		}}),
		barkHTTPClient: client,
	}
	message := receivedSMS{Sender: "10086", Content: "验证码 4321", Timestamp: time.Now()}
	if newCount, _ := instance.mergeSMS([]receivedSMS{message}); newCount != 1 {
		t.Fatalf("first merge newCount = %d, want 1", newCount)
	}
	if newCount, _ := instance.mergeSMS([]receivedSMS{message}); newCount != 0 {
		t.Fatalf("duplicate merge newCount = %d, want 0", newCount)
	}
	instance.recordSMS("已发送至 10086", "查询余额", time.Now())

	select {
	case path := <-received:
		if path != "/[主卡]验证码 4321" {
			t.Fatalf("forwarded path = %q", path)
		}
	case <-time.After(time.Second):
		t.Fatal("new incoming SMS was not forwarded")
	}
	assertNoBarkRequest(t, received)
}

func TestMissedCallForwardsOnlyAfterUnansweredCallEnds(t *testing.T) {
	received := make(chan string, 2)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		received <- r.URL.Path
		return barkSuccessResponse(r), nil
	})}
	instance := &app{
		barkSettingsLoaded: true,
		barkSettings: normalizeBarkSettings(barkSettings{MissedCall: barkChannelSettings{
			Enabled: true, APIURL: "https://example.invalid/{message}",
			MessageTemplate: "未接：{number}",
		}}),
		barkHTTPClient: client,
	}
	started := time.Date(2026, 8, 10, 14, 0, 0, 0, time.Local)
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10010"}}, started)
	assertNoBarkRequest(t, received)
	instance.applyCallPoll(nil, started.Add(8*time.Second))
	select {
	case path := <-received:
		if path != "/未接：10010" {
			t.Fatalf("forwarded path = %q", path)
		}
	case <-time.After(time.Second):
		t.Fatal("missed call was not forwarded")
	}
	instance.applyCallPoll(nil, started.Add(11*time.Second))
	assertNoBarkRequest(t, received)
}

func TestAnsweredCallDoesNotForwardBark(t *testing.T) {
	received := make(chan string, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		received <- r.URL.Path
		return barkSuccessResponse(r), nil
	})}
	instance := &app{
		barkSettingsLoaded: true,
		barkSettings: normalizeBarkSettings(barkSettings{MissedCall: barkChannelSettings{
			Enabled: true, APIURL: "https://example.invalid/{message}", MessageTemplate: "未接：{number}",
		}}),
		barkHTTPClient: client,
	}
	started := time.Date(2026, 8, 10, 15, 0, 0, 0, time.Local)
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10086"}}, started)
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "active", Number: "10086"}}, started.Add(3*time.Second))
	instance.applyCallPoll(nil, started.Add(12*time.Second))
	assertNoBarkRequest(t, received)
}

func TestVoiceAgentCallForwardsBarkAfterCallEnds(t *testing.T) {
	received := make(chan string, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		received <- r.URL.Path
		return barkSuccessResponse(r), nil
	})}
	instance := &app{
		demo:               true,
		barkSettingsLoaded: true,
		barkSettings: normalizeBarkSettings(barkSettings{MissedCall: barkChannelSettings{
			Enabled: true, APIURL: "https://example.invalid/{message}", MessageTemplate: "{status}：{number}",
		}}),
		barkHTTPClient: client,
	}
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen"}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	started := time.Date(2026, 8, 10, 16, 0, 0, 0, time.Local)
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10010"}}, started)
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "active", Number: "10010"}}, started.Add(3*time.Second))
	instance.applyCallPoll(nil, started.Add(12*time.Second))

	select {
	case path := <-received:
		if path != "/AI 已接听：10010" {
			t.Fatalf("forwarded path = %q", path)
		}
	case <-time.After(time.Second):
		t.Fatal("AI-handled call was not forwarded")
	}
}

func barkSuccessResponse(request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"code":200}`)),
		Request:    request,
	}
}

func putBarkSettings(t *testing.T, instance *app, path, body string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT %s status = %d, body = %s", path, response.Code, response.Body.String())
	}
}

func getBarkSettings(t *testing.T, instance *app, path string) barkChannelSettings {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
	}
	var got barkChannelSettings
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode Bark settings: %v", err)
	}
	return got
}

func assertNoBarkRequest(t *testing.T, requests <-chan string) {
	t.Helper()
	select {
	case path := <-requests:
		t.Fatalf("unexpected Bark forwarding: %q", path)
	case <-time.After(150 * time.Millisecond):
	}
}
