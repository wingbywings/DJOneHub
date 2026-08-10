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

func TestFormatBarkMessage(t *testing.T) {
	tests := []struct {
		alias   string
		content string
		want    string
	}{
		{alias: "备用卡", content: "验证码 1234", want: "[备用卡]验证码 1234"},
		{alias: "[主卡]", content: "余额不足", want: "[主卡]余额不足"},
		{alias: "", content: "普通短信", want: "普通短信"},
	}
	for _, test := range tests {
		if got := formatBarkMessage(test.alias, test.content); got != test.want {
			t.Fatalf("formatBarkMessage(%q, %q) = %q, want %q", test.alias, test.content, got, test.want)
		}
	}
}

func TestValidateBarkSettings(t *testing.T) {
	if err := validateBarkSettings(barkSettings{Enabled: true}); err == nil {
		t.Fatal("enabled Bark settings without an API URL should fail validation")
	}
	if err := validateBarkSettings(barkSettings{Enabled: true, APIURL: "https://api.day.app/key/no-placeholder"}); err == nil {
		t.Fatal("Bark API URL without {message} should fail validation")
	}
	if err := validateBarkSettings(barkSettings{Enabled: true, APIURL: "https://api.day.app/key/{message}?group=短信通知"}); err != nil {
		t.Fatalf("valid Bark settings failed validation: %v", err)
	}
	if err := validateBarkSettings(barkSettings{}); err != nil {
		t.Fatalf("empty disabled Bark settings failed validation: %v", err)
	}
}

func TestSendBarkNotificationEncodesMessageAndPreservesGroup(t *testing.T) {
	received := make(chan struct{}, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/[备用卡]验证码 1234/5" {
			t.Errorf("path = %q, want decoded Bark message", r.URL.Path)
		}
		if got := r.URL.Query().Get("group"); got != "短信通知" {
			t.Errorf("group = %q, want 短信通知", got)
		}
		received <- struct{}{}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":200,"message":"success"}`)),
			Request:    r,
		}, nil
	})}

	settings := barkSettings{
		Enabled: true,
		APIURL:  "https://example.invalid/{message}?group=短信通知",
		Alias:   "备用卡",
	}
	if err := sendBarkNotification(context.Background(), client, settings, "验证码 1234/5"); err != nil {
		t.Fatalf("sendBarkNotification() error = %v", err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("Bark server did not receive the notification")
	}
}

func TestBarkSettingsAPIPersistsConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bark-settings.json")
	instance := &app{barkSettingsPath: path}
	body := `{"enabled":true,"api_url":"https://api.day.app/key/{message}?group=短信通知","alias":"备用卡"}`
	request := httptest.NewRequest(http.MethodPut, "/api/settings/bark", strings.NewReader(body))
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings/bark status = %d, body = %s", response.Code, response.Body.String())
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("saved Bark config stat error = %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("saved Bark config mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := &app{barkSettingsPath: path}
	request = httptest.NewRequest(http.MethodGet, "/api/settings/bark", nil)
	response = httptest.NewRecorder()
	reloaded.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/bark status = %d, body = %s", response.Code, response.Body.String())
	}
	var got barkSettings
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode saved Bark settings: %v", err)
	}
	if !got.Enabled || got.Alias != "备用卡" || got.APIURL == "" {
		t.Fatalf("saved Bark settings = %#v", got)
	}
}

func TestMergeSMSForwardsOnlyNewIncomingMessages(t *testing.T) {
	received := make(chan string, 3)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		received <- r.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":200}`)),
			Request:    r,
		}, nil
	})}

	instance := &app{
		barkSettingsLoaded: true,
		barkSettings: barkSettings{
			Enabled: true,
			APIURL:  "https://example.invalid/{message}",
			Alias:   "主卡",
		},
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
	select {
	case path := <-received:
		t.Fatalf("unexpected duplicate/outgoing Bark forwarding: %q", path)
	case <-time.After(150 * time.Millisecond):
	}
}
