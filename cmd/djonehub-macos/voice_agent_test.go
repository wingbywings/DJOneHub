package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

type trackedVoiceAgentSession struct {
	fakeVoiceAgentSession
	closed chan struct{}
}

func (f *trackedVoiceAgentSession) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

func TestIncomingCallPreparesVoiceAgentSessionBeforeActivation(t *testing.T) {
	instance := &app{barkSettingsLoaded: true, callPollInterval: time.Second}
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen", Revision: 7}
	opened := make(chan voiceagent.SessionConfig, 1)
	session := &trackedVoiceAgentSession{closed: make(chan struct{})}
	controller.openSessionOverride = func(_ context.Context, state voiceAgentState, config voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
		opened <- config
		return session, state, nil
	}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	now := time.Now()
	instance.applyCallPoll([]parsedCall{{Index: 3, Direction: "incoming", State: "incoming"}}, now)
	select {
	case config := <-opened:
		if config.ID == "" || config.OpeningPrompt == "" {
			t.Fatalf("prepared config = %#v", config)
		}
	case <-time.After(time.Second):
		t.Fatal("voice agent session was not prepared while the call was ringing")
	}
	instance.callMu.RLock()
	callID := instance.activeCall.ID
	instance.callMu.RUnlock()
	prepared, _, cancel, found, err := controller.takePreparedSession(context.Background(), callID, 7)
	if err != nil || !found || prepared != session {
		t.Fatalf("prepared session = %v, found=%v, err=%v", prepared, found, err)
	}
	cancel()
	_ = prepared.Close()
}

func TestEndedCallClosesPreparedVoiceAgentSession(t *testing.T) {
	instance := &app{barkSettingsLoaded: true, callPollInterval: time.Second}
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen", Revision: 1}
	opened := make(chan struct{}, 1)
	session := &trackedVoiceAgentSession{closed: make(chan struct{})}
	controller.openSessionOverride = func(_ context.Context, state voiceAgentState, _ voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
		opened <- struct{}{}
		return session, state, nil
	}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	now := time.Now()
	instance.applyCallPoll([]parsedCall{{Index: 3, Direction: "incoming", State: "incoming"}}, now)
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("voice agent session was not prepared")
	}
	instance.applyCallPoll(nil, now.Add(time.Second))
	select {
	case <-session.closed:
	case <-time.After(time.Second):
		t.Fatal("prepared voice agent session remained open after the call ended")
	}
}

func TestConnectedCallDoesNotPrepareDuplicateVoiceAgentSession(t *testing.T) {
	instance := &app{barkSettingsLoaded: true}
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen", Revision: 2, Connected: true, CallID: "call-1"}
	opened := make(chan struct{}, 1)
	controller.openSessionOverride = func(_ context.Context, state voiceAgentState, _ voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
		opened <- struct{}{}
		return &trackedVoiceAgentSession{closed: make(chan struct{})}, state, nil
	}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	now := time.Now()
	instance.activeCall = &callRecord{ID: "call-1", State: "active", Direction: "incoming", StartedAt: now, UpdatedAt: now}
	instance.syncVoiceAgentPreparation()
	select {
	case <-opened:
		t.Fatal("connected media session started a duplicate prepared provider session")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProfileChangePreparesReplacementWhileOldSessionIsConnected(t *testing.T) {
	instance := &app{barkSettingsLoaded: true}
	controller := newVoiceAgentController("")
	state := voiceAgentState{Enabled: true, Provider: "qwen", Revision: 3, Connected: true, CallID: "call-1"}
	controller.state = state
	opened := make(chan voiceagent.SessionConfig, 1)
	controller.openSessionOverride = func(_ context.Context, profile voiceAgentState, config voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
		opened <- config
		return &trackedVoiceAgentSession{closed: make(chan struct{})}, profile, nil
	}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	now := time.Now()
	instance.activeCall = &callRecord{ID: "call-1", State: "active", Direction: "incoming", StartedAt: now, UpdatedAt: now}
	instance.prepareVoiceAgentForCurrentCall(state, true)
	select {
	case config := <-opened:
		if config.ID != "call-1" {
			t.Fatalf("replacement config = %#v", config)
		}
	case <-time.After(time.Second):
		t.Fatal("profile change did not prepare a replacement provider session")
	}
	controller.discardPreparedSession("")
}

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

func TestVoiceAgentAuditStoresOnlyCompleteAIReplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice-agent.json")
	controller := newVoiceAgentController(path)
	controller.state.AuditEnabled = true

	controller.runtime.mu.Lock()
	controller.runtime.nextSubscriber++
	subscriberID := controller.runtime.nextSubscriber
	events := make(chan voiceAgentAuditEvent, 2)
	controller.runtime.subscribers[subscriberID] = events
	controller.runtime.mu.Unlock()

	controller.recordEvent("call-1", voiceagent.Event{Type: voiceagent.EventOutputTranscriptDelta, Provider: "minimax", Text: "第一段", At: time.Now()})
	controller.recordEvent("call-1", voiceagent.Event{Type: voiceagent.EventOutputTranscriptFinal, Provider: "minimax", Text: "第一段完整回复。", At: time.Now()})

	wantTypes := []voiceagent.EventType{voiceagent.EventOutputTranscriptDelta, voiceagent.EventOutputTranscriptFinal}
	for _, wantType := range wantTypes {
		select {
		case event := <-events:
			if event.Type != wantType {
				t.Fatalf("live event type = %q, want %q", event.Type, wantType)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for live transcript event")
		}
	}

	controller.runtime.mu.Lock()
	audit := append([]voiceAgentAuditEvent(nil), controller.runtime.audit...)
	controller.runtime.mu.Unlock()
	if len(audit) != 1 || audit[0].Type != voiceagent.EventOutputTranscriptFinal || audit[0].Text != "第一段完整回复。" {
		t.Fatalf("audit = %#v", audit)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "voice-agent-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"type":"transcript.output.delta"`)) {
		t.Fatalf("audit persisted an intermediate AI reply: %s", data)
	}
	if !bytes.Contains(data, []byte("第一段完整回复。")) {
		t.Fatalf("audit did not persist the complete AI reply: %s", data)
	}
	auditPath := filepath.Join(filepath.Dir(path), "voice-agent-audit.jsonl")
	if err := appendVoiceAgentAudit(auditPath, voiceAgentAuditEvent{Type: voiceagent.EventOutputTranscriptDelta, Text: "历史中间片段", Persisted: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reloaded := newVoiceAgentController(path)
	reloaded.runtime.mu.Lock()
	reloadedAudit := append([]voiceAgentAuditEvent(nil), reloaded.runtime.audit...)
	reloaded.runtime.mu.Unlock()
	if len(reloadedAudit) != 1 || reloadedAudit[0].Type != voiceagent.EventOutputTranscriptFinal {
		t.Fatalf("reloaded audit = %#v", reloadedAudit)
	}
}

func TestVoiceAgentAuditServesRecordingFromRecordingDirectory(t *testing.T) {
	recordingRoot := t.TempDir()
	recordingPath := filepath.Join(recordingRoot, "call.wav")
	wav := []byte("RIFF\x04\x00\x00\x00WAVE")
	if err := os.WriteFile(recordingPath, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := newVoiceAgentController("")
	controller.recordEvent("call-1", voiceagent.Event{Type: "recording.stopped", Text: recordingPath, At: time.Now()})
	controller.runtime.mu.Lock()
	sequence := controller.runtime.audit[0].Sequence
	controller.runtime.mu.Unlock()
	instance := newDemoApp()
	instance.recordingRoot = recordingRoot
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/voice-agent/audit/%d/recording", sequence), nil)
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), wav) {
		t.Fatalf("recording status=%d body=%q", response.Code, response.Body.Bytes())
	}
	if response.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}

	outsidePath := filepath.Join(t.TempDir(), "outside.wav")
	if err := os.WriteFile(outsidePath, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	controller.recordEvent("call-1", voiceagent.Event{Type: "recording.stopped", Text: outsidePath, At: time.Now()})
	controller.runtime.mu.Lock()
	outsideSequence := controller.runtime.audit[len(controller.runtime.audit)-1].Sequence
	controller.runtime.mu.Unlock()
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/voice-agent/audit/%d/recording", outsideSequence), nil)
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || bytes.Contains(response.Body.Bytes(), []byte(outsidePath)) {
		t.Fatalf("outside recording status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestVoiceAgentAuditPreservesUsageDetails(t *testing.T) {
	controller := newVoiceAgentController("")
	controller.recordEvent("call-1", voiceagent.Event{
		Type:     voiceagent.EventUsage,
		Provider: "qwen",
		Usage:    map[string]any{"input_tokens": 12, "output_tokens": 8, "total_tokens": 20},
		At:       time.Now(),
	})
	controller.runtime.mu.Lock()
	audit := append([]voiceAgentAuditEvent(nil), controller.runtime.audit...)
	controller.runtime.mu.Unlock()
	if len(audit) != 1 || audit[0].Usage["total_tokens"] != 20 {
		t.Fatalf("usage audit = %#v", audit)
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
	call := &voiceagent.ToolCall{ID: "pending-1", Name: "send_dtmf", Arguments: json.RawMessage(`{"digit":"1"}`)}
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

func TestVoiceAgentHangUpExecutesWithoutOperatorDecision(t *testing.T) {
	instance := newDemoApp()
	controller := newVoiceAgentController("")
	controller.state = voiceAgentState{Enabled: true, Provider: "qwen", ToolsEnabled: true, RedactPII: true}
	instance.voiceAgentOnce.Do(func() { instance.voiceAgent = controller })
	now := time.Now()
	instance.activeCall = &callRecord{ID: "call-1", State: "active", Direction: "incoming", StartedAt: now, UpdatedAt: now}
	session := &fakeVoiceAgentSession{results: make(chan map[string]any, 1)}
	call := &voiceagent.ToolCall{ID: "hang-up-1", Name: "hang_up_call", Arguments: json.RawMessage(`{}`)}

	instance.handleVoiceAgentTool(controller, session, "call-1", call)

	select {
	case result := <-session.results:
		if result["ok"] != true {
			t.Fatalf("result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("hang-up result was not returned to model")
	}
	controller.runtime.mu.Lock()
	pendingCount := len(controller.runtime.pendingTools)
	controller.runtime.mu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pending tools = %d, want 0", pendingCount)
	}
	if instance.activeCall != nil {
		t.Fatalf("active call = %#v, want nil", instance.activeCall)
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
	instance.callMu.RLock()
	call := instance.activeCall
	if call != nil {
		copy := *call
		call = &copy
	}
	instance.callMu.RUnlock()
	if call == nil || call.State != "active" || !call.AIHandled {
		t.Fatalf("call = %#v", call)
	}
	instance.audioHostMu.RLock()
	wantRecording := instance.audioHost.WantRecording
	instance.audioHostMu.RUnlock()
	if !wantRecording {
		t.Fatal("AI auto-answer did not enable recording")
	}
}
