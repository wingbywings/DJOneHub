package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func TestVoiceAgentCallsPaginationAndSearch(t *testing.T) {
	controller := newVoiceAgentController("")
	started := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	for index := range 65 {
		controller.runtime.callRecords = append(controller.runtime.callRecords, voiceAgentCallRecord{
			CallID: fmt.Sprintf("call-%02d", index), Number: fmt.Sprintf("1380013%04d", index),
			StartedAt: started.Add(time.Duration(index) * time.Minute),
			Messages:  []voiceAgentCallMessage{{Role: "caller", Text: "普通通话"}},
		})
	}
	// The search must include records older than the legacy 50-record window,
	// and either side of the conversation, beyond the last preview message.
	controller.runtime.callRecords[0].Messages = []voiceAgentCallMessage{
		{Role: "caller", Text: "请安排明天的快递 DELIVERY"},
		{Role: "ai", Text: "好的"},
	}
	controller.runtime.callRecords[12].Messages = []voiceAgentCallMessage{{Role: "ai", Text: "快递将在明天送达"}}
	instance := newDemoApp()
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	routes := instance.routes()
	tests := []struct {
		name       string
		query      string
		page       int
		limit      int
		total      int
		totalPages int
		count      int
		first      string
		last       string
	}{
		{name: "legacy default", limit: 50, page: 1, total: 65, totalPages: 2, count: 50, first: "call-64", last: "call-15"},
		{name: "first page", query: "limit=10", limit: 10, page: 1, total: 65, totalPages: 7, count: 10, first: "call-64", last: "call-55"},
		{name: "second page", query: "limit=10&page=2", limit: 10, page: 2, total: 65, totalPages: 7, count: 10, first: "call-54", last: "call-45"},
		{name: "partial last page", query: "limit=10&page=7", limit: 10, page: 7, total: 65, totalPages: 7, count: 5, first: "call-04", last: "call-00"},
		{name: "clamped page", query: "limit=10&page=9223372036854775807", limit: 10, page: 7, total: 65, totalPages: 7, count: 5, first: "call-04", last: "call-00"},
		{name: "phone substring", query: "q=13800130000", limit: 50, page: 1, total: 1, totalPages: 1, count: 1, first: "call-00", last: "call-00"},
		{name: "both transcript roles", query: "q=" + url.QueryEscape("快递"), limit: 50, page: 1, total: 2, totalPages: 1, count: 2, first: "call-12", last: "call-00"},
		{name: "filter before pagination", query: "q=" + url.QueryEscape("快递") + "&limit=1&page=2", limit: 1, page: 2, total: 2, totalPages: 2, count: 1, first: "call-00", last: "call-00"},
		{name: "case and whitespace", query: "q=" + url.QueryEscape("  delivery  "), limit: 50, page: 1, total: 1, totalPages: 1, count: 1, first: "call-00", last: "call-00"},
		{name: "empty keyword", query: "q=+++&limit=100", limit: 100, page: 1, total: 65, totalPages: 1, count: 65, first: "call-64", last: "call-00"},
		{name: "no matches", query: "q=not-found&page=8", limit: 50, page: 1},
		{name: "invalid parameters", query: "limit=bad&page=-2", limit: 50, page: 1, total: 65, totalPages: 2, count: 50, first: "call-64", last: "call-15"},
		{name: "zero parameters", query: "limit=0&page=0", limit: 50, page: 1, total: 65, totalPages: 2, count: 50, first: "call-64", last: "call-15"},
		{name: "bounded limit", query: "limit=10000", limit: 100, page: 1, total: 65, totalPages: 1, count: 65, first: "call-64", last: "call-00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/voice-agent/calls?"+test.query, nil))
			var result struct {
				Calls      []voiceAgentCallView `json:"calls"`
				Count      int                  `json:"count"`
				Total      int                  `json:"total"`
				Page       int                  `json:"page"`
				Limit      int                  `json:"limit"`
				TotalPages int                  `json:"total_pages"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if result.Page != test.page || result.Limit != test.limit || result.Total != test.total || result.TotalPages != test.totalPages || result.Count != test.count || len(result.Calls) != test.count || result.Calls == nil {
				t.Fatalf("unexpected pagination: %s", response.Body.String())
			}
			if test.count > 0 && (result.Calls[0].CallID != test.first || result.Calls[test.count-1].CallID != test.last) {
				t.Fatalf("unexpected call order: %s", response.Body.String())
			}
		})
	}
}

func TestVoiceAgentCallsSearchThroughDeviceRoute(t *testing.T) {
	controller := newVoiceAgentController("")
	controller.runtime.callRecords = []voiceAgentCallRecord{
		{CallID: "device-call", Number: "13800138000", Messages: []voiceAgentCallMessage{{Role: "caller", Text: "设备内转写"}}},
	}
	runtime := newDemoApp()
	runtime.deviceID = "device-search"
	runtime.voiceAgentOnce.Do(func() { runtime.voiceAgent = controller })
	hub := &usbDeviceHub{
		devices: map[string]*managedUSBDevice{
			"device-search": {ID: "device-search", State: "ready", Runtime: runtime},
		},
		fallback: newDemoApp(),
	}
	response := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/devices/device-search/voice-agent/calls?limit=1&q="+url.QueryEscape("设备内"), nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"call_id":"device-call"`) || !strings.Contains(response.Body.String(), `"total":1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestVoiceAgentCallRecordPersistsBothSidesWithoutAuditRedaction(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "voice-agent.json")
	controller := newVoiceAgentController(settingsPath)
	controller.state.RedactPII = true
	started := time.Date(2026, 8, 24, 10, 0, 0, 0, time.Local)
	controller.startCallRecord(callRecord{
		ID: "call-archive-1", Direction: "incoming", Number: "13800138000", StartedAt: started,
	}, "qwen")
	controller.recordEvent("call-archive-1", voiceagent.Event{
		Type: voiceagent.EventInputTranscriptFinal, Provider: "qwen",
		Text: "我的电话是 13800138000", At: started.Add(time.Second),
	})
	controller.recordEvent("call-archive-1", voiceagent.Event{
		Type: voiceagent.EventOutputTranscriptFinal, Provider: "qwen",
		Text: "好的，已经为您记录。", At: started.Add(2 * time.Second),
	})
	controller.finishCallRecord("call-archive-1", started.Add(10*time.Second))

	data, err := os.ReadFile(filepath.Join(filepath.Dir(settingsPath), "voice-agent-calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("13800138000")) || !bytes.Contains(data, []byte("已经为您记录")) {
		t.Fatalf("call archive did not preserve both sides: %s", data)
	}

	reloaded := newVoiceAgentController(settingsPath)
	reloaded.runtime.mu.Lock()
	records := cloneVoiceAgentCallRecords(reloaded.runtime.callRecords)
	reloaded.runtime.mu.Unlock()
	if len(records) != 1 || records[0].EndedAt == nil || len(records[0].Messages) != 2 {
		t.Fatalf("reloaded records = %#v", records)
	}
	if records[0].Messages[0].Role != "caller" || records[0].Messages[1].Role != "ai" {
		t.Fatalf("messages = %#v", records[0].Messages)
	}
}

func TestVoiceAgentCallAPIPlaysAndDownloadsFinalRecording(t *testing.T) {
	recordingRoot := t.TempDir()
	recordingPath := filepath.Join(recordingRoot, "完整通话.wav")
	wav := []byte("RIFF\x04\x00\x00\x00WAVE")
	if err := os.WriteFile(recordingPath, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := newVoiceAgentController("")
	started := time.Now().Add(-time.Minute)
	controller.startCallRecord(callRecord{ID: "call-recording-1", Direction: "incoming", StartedAt: started}, "qwen")
	controller.recordEvent("call-recording-1", voiceagent.Event{Type: "recording.stopped", Text: recordingPath, At: time.Now()})
	controller.finishCallRecord("call-recording-1", time.Now())
	instance := newDemoApp()
	instance.recordingRoot = recordingRoot
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })

	listResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/voice-agent/calls", nil))
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), recordingRoot) {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var list struct {
		Calls []voiceAgentCallView `json:"calls"`
	}
	if json.Unmarshal(listResponse.Body.Bytes(), &list) != nil || len(list.Calls) != 1 || !list.Calls[0].RecordingAvailable {
		t.Fatalf("list body=%s", listResponse.Body.String())
	}

	playResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(playResponse, httptest.NewRequest(http.MethodGet, "/api/voice-agent/calls/call-recording-1/recording", nil))
	if playResponse.Code != http.StatusOK || !bytes.Equal(playResponse.Body.Bytes(), wav) || !strings.HasPrefix(playResponse.Header().Get("Content-Disposition"), "inline;") {
		t.Fatalf("play status=%d disposition=%q body=%q", playResponse.Code, playResponse.Header().Get("Content-Disposition"), playResponse.Body.Bytes())
	}

	downloadResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(downloadResponse, httptest.NewRequest(http.MethodGet, "/api/voice-agent/calls/call-recording-1/recording?download=1", nil))
	if downloadResponse.Code != http.StatusOK || !strings.HasPrefix(downloadResponse.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download status=%d disposition=%q", downloadResponse.Code, downloadResponse.Header().Get("Content-Disposition"))
	}
}

func TestVoiceAgentCallRecordingSupportsRangeThroughDeviceRoute(t *testing.T) {
	recordingRoot := t.TempDir()
	recordingPath := filepath.Join(recordingRoot, "range.wav")
	wav := []byte("RIFF\x10\x00\x00\x00WAVEfmt payload")
	if err := os.WriteFile(recordingPath, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := newVoiceAgentController("")
	controller.startCallRecord(callRecord{ID: "call-range-1", Direction: "incoming", StartedAt: time.Now()}, "qwen")
	controller.recordEvent("call-range-1", voiceagent.Event{Type: "recording.stopped", Text: recordingPath, At: time.Now()})
	runtime := newDemoApp()
	runtime.deviceID = "device-range"
	runtime.recordingRoot = recordingRoot
	runtime.voiceAgentOnce.Do(func() { runtime.voiceAgent = controller })
	hub := &usbDeviceHub{
		devices: map[string]*managedUSBDevice{
			"device-range": {ID: "device-range", State: "ready", Runtime: runtime},
		},
		fallback: newDemoApp(),
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/devices/device-range/voice-agent/calls/call-range-1/recording", nil)
	request.Header.Set("Range", "bytes=0-7")
	hub.routes(webAssets).ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), wav[:8]) {
		t.Fatalf("range status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.Bytes())
	}
}

func TestAudioHostAssociatesStoppedRecordingAfterCallDisappears(t *testing.T) {
	controller := newVoiceAgentController("")
	started := time.Now()
	call := callRecord{ID: "call-finished-before-host", Direction: "incoming", State: "active", StartedAt: started}
	controller.startCallRecord(call, "qwen")
	instance := newDemoApp()
	instance.activeCall = &call
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })

	recordingPath := filepath.Join(t.TempDir(), "call.wav")
	startBody := strings.NewReader(`{"enabled":true,"running":true,"recording":true,"recording_path":"` + recordingPath + `"}`)
	startResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(startResponse, httptest.NewRequest(http.MethodPost, "/api/calls/audio/host/register", startBody))
	if startResponse.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", startResponse.Code, startResponse.Body.String())
	}
	instance.callMu.Lock()
	instance.activeCall = nil
	instance.callMu.Unlock()
	stopResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(stopResponse, httptest.NewRequest(http.MethodPost, "/api/calls/audio/host/register", strings.NewReader(`{"enabled":true,"running":false,"recording":false}`)))
	if stopResponse.Code != http.StatusOK {
		t.Fatalf("stop status=%d body=%s", stopResponse.Code, stopResponse.Body.String())
	}

	controller.runtime.mu.Lock()
	records := cloneVoiceAgentCallRecords(controller.runtime.callRecords)
	controller.runtime.mu.Unlock()
	if len(records) != 1 || records[0].RecordingPath != recordingPath {
		t.Fatalf("records = %#v", records)
	}
}
