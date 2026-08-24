package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

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
