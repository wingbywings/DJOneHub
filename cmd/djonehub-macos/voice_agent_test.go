package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

type fakeVoiceAgentSession struct {
	results chan map[string]any
}

func (f *fakeVoiceAgentSession) SendAudio(context.Context, []byte) error { return nil }
func (f *fakeVoiceAgentSession) Events() <-chan voiceagent.Event {
	return make(chan voiceagent.Event)
}
func (f *fakeVoiceAgentSession) SubmitToolResult(_ context.Context, _ string, output any) error {
	f.results <- output.(map[string]any)
	return nil
}
func (f *fakeVoiceAgentSession) Close() error { return nil }

func TestVoiceAgentStatusDoesNotExposeCredentials(t *testing.T) {
	instance := newDemoApp()
	instance.voiceAgentSettingsPath = filepath.Join(t.TempDir(), "voice-agent.json")
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/voice-agent/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if bytes.Contains(bytes.ToLower(response.Body.Bytes()), []byte("api_key")) {
		t.Fatalf("status exposed credential field: %s", response.Body.String())
	}
	var result struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 3 {
		t.Fatalf("providers = %#v", result.Providers)
	}
}

func TestVoiceAgentNormalizesProviderSpecificVoice(t *testing.T) {
	if got := voiceForProvider("openai", "Cherry"); got != "marin" {
		t.Fatalf("OpenAI voice = %q, want marin", got)
	}
	if got := voiceForProvider("openai", "CEDAR"); got != "cedar" {
		t.Fatalf("OpenAI voice = %q, want cedar", got)
	}
	if got := voiceForProvider("qwen", "marin"); got != "Cherry" {
		t.Fatalf("Qwen voice = %q, want Cherry", got)
	}
}

func TestVoiceAgentCanBeExplicitlyDisabledWithoutCredentials(t *testing.T) {
	instance := newDemoApp()
	instance.voiceAgentSettingsPath = filepath.Join(t.TempDir(), "voice-agent.json")
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/voice-agent/config", bytes.NewBufferString(`{"enabled":false,"provider":"qwen"}`))
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if instance.ensureVoiceAgent().snapshot().Enabled {
		t.Fatal("agent remained enabled")
	}
}

func TestVoiceAgentProfilePersistsMiniMaxSTTSelection(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "minimax-secret")
	t.Setenv("DASHSCOPE_API_KEY", "qwen-secret")
	path := filepath.Join(t.TempDir(), "voice-agent.json")
	instance := newDemoApp()
	instance.voiceAgentSettingsPath = path
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/voice-agent/config", bytes.NewBufferString(`{"enabled":true,"provider":"minimax","fallback_provider":"openai","model":"MiniMax-M2.7-highspeed","voice":"male-qn-qingse","stt_provider":"qwen","fallback_stt_provider":"openai","stt_model":"qwen3-asr-flash-realtime","tools_enabled":true,"audit_enabled":true,"redact_pii":true,"auto_answer":true,"auto_answer_delay_ms":900}`))
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	reloaded := newVoiceAgentController(path)
	state := reloaded.snapshot()
	if !state.Enabled || state.Provider != "minimax" || state.FallbackProvider != "openai" || state.STTProvider != "qwen" || state.FallbackSTT != "openai" || state.STTModel != "qwen3-asr-flash-realtime" || !state.ToolsEnabled || !state.AuditEnabled || !state.RedactPII || !state.AutoAnswer || state.AutoAnswerDelayMS != 900 {
		t.Fatalf("reloaded state = %#v", state)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("minimax-secret")) || bytes.Contains(data, []byte("qwen-secret")) {
		t.Fatalf("profile persisted an API key: %s", data)
	}
}

func TestVoiceAgentAuditRedactsPersistsAndClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice-agent.json")
	controller := newVoiceAgentController(path)
	controller.state.AuditEnabled = true
	controller.state.RedactPII = true
	controller.recordEvent("call-1", voiceagent.Event{Type: voiceagent.EventInputTranscriptFinal, Provider: "qwen", Text: "请联系 13800138000 或 test@example.com", At: time.Now()})
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "voice-agent-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("13800138000")) || bytes.Contains(data, []byte("test@example.com")) {
		t.Fatalf("audit was not redacted: %s", data)
	}
	instance := newDemoApp()
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/voice-agent/audit", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("138****8000")) {
		t.Fatalf("audit status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	instance.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/voice-agent/audit", bytes.NewBufferString(`{"confirm":true}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "voice-agent-audit.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("audit file still exists: %v", err)
	}
}

func TestVoiceAgentProviderFailureMovesFallbackFirst(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "qwen-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	controller := newVoiceAgentController("")
	state := voiceAgentState{Provider: "qwen", FallbackProvider: "openai"}
	controller.markProviderFailure("qwen", os.ErrDeadlineExceeded)
	candidates := controller.providerCandidates(state)
	if len(candidates) != 2 || candidates[0].Provider != "openai" || candidates[1].Provider != "qwen" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestVoiceAgentSensitiveToolRequiresOperatorDecision(t *testing.T) {
	instance := newDemoApp()
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen", ToolsEnabled: true, RedactPII: true}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	session := &fakeVoiceAgentSession{results: make(chan map[string]any, 1)}
	call := &voiceagent.ToolCall{ID: "pending-1", Name: "hang_up_call", Arguments: json.RawMessage(`{}`)}
	instance.handleVoiceAgentTool(controller, session, "call-1", call)
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/voice-agent/tools/pending", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("pending-1")) {
		t.Fatalf("pending status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	instance.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/voice-agent/tools/pending-1", bytes.NewBufferString(`{"approve":false}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("decision status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case result := <-session.results:
		if result["ok"] != false {
			t.Fatalf("result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("tool rejection was not returned to model")
	}
}

func TestVoiceAgentAutoAnswerRechecksRingingCall(t *testing.T) {
	instance := newDemoApp()
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen", AutoAnswer: true, AutoAnswerDelayMS: 10}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	now := time.Now()
	instance.activeCall = &callRecord{ID: "call-auto", State: "incoming", Direction: "incoming", StartedAt: now, UpdatedAt: now}
	instance.scheduleVoiceAgentAutoAnswer("", "incoming")
	time.Sleep(80 * time.Millisecond)
	if instance.activeCall == nil || instance.activeCall.State != "active" {
		t.Fatalf("call = %#v", instance.activeCall)
	}
}
