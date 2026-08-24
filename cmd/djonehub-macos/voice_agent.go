package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
	"github.com/iniwex5/vohive/internal/voiceagent/providers/minimax"
	openaiProvider "github.com/iniwex5/vohive/internal/voiceagent/providers/openai"
	qwenProvider "github.com/iniwex5/vohive/internal/voiceagent/providers/qwen"
)

type voiceAgentState struct {
	Enabled           bool      `json:"enabled"`
	Provider          string    `json:"provider"`
	FallbackProvider  string    `json:"fallback_provider,omitempty"`
	Model             string    `json:"model,omitempty"`
	Voice             string    `json:"voice,omitempty"`
	Instructions      string    `json:"instructions,omitempty"`
	STTProvider       string    `json:"stt_provider,omitempty"`
	FallbackSTT       string    `json:"fallback_stt_provider,omitempty"`
	STTModel          string    `json:"stt_model,omitempty"`
	ToolsEnabled      bool      `json:"tools_enabled"`
	AuditEnabled      bool      `json:"audit_enabled"`
	RedactPII         bool      `json:"redact_pii"`
	AutoAnswer        bool      `json:"auto_answer"`
	AutoAnswerDelayMS int       `json:"auto_answer_delay_ms"`
	Connected         bool      `json:"connected"`
	CallID            string    `json:"call_id,omitempty"`
	ActiveProvider    string    `json:"active_provider,omitempty"`
	ReconnectCount    int       `json:"reconnect_count"`
	Revision          uint64    `json:"revision"`
	LastError         string    `json:"last_error,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (s voiceAgentState) MediaMode() string {
	if s.Enabled {
		return "agent"
	}
	return "manual"
}

type voiceAgentController struct {
	mu                  sync.RWMutex
	registry            *voiceagent.Registry
	state               voiceAgentState
	keys                map[string]bool
	transcribers        map[string]cascade.Transcriber
	minimaxTextConfig   minimax.TextConfig
	minimaxSpeechConfig minimax.SpeechConfig
	settingsPath        string
	mediaToken          string
	sessionMu           sync.Mutex
	preparedMu          sync.Mutex
	prepared            *preparedVoiceAgentSession
	openSessionOverride func(context.Context, voiceAgentState, voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error)
	runtime             *voiceAgentRuntime
}

type preparedVoiceAgentSession struct {
	callID   string
	revision uint64
	ready    chan struct{}
	cancel   context.CancelFunc
	session  voiceagent.Session
	profile  voiceAgentState
	err      error
}

func newVoiceAgentController(settingsPath string) *voiceAgentController {
	registry := voiceagent.NewRegistry()
	keys := map[string]bool{
		"qwen":    strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY")) != "",
		"openai":  strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "",
		"minimax": strings.TrimSpace(os.Getenv("MINIMAX_API_KEY")) != "",
	}
	qwen, _ := qwenProvider.New(qwenProvider.Config{APIKey: os.Getenv("DASHSCOPE_API_KEY"), Endpoint: os.Getenv("QWEN_REALTIME_ENDPOINT"), Model: os.Getenv("QWEN_REALTIME_MODEL")})
	openai, _ := openaiProvider.New(openaiProvider.Config{APIKey: os.Getenv("OPENAI_API_KEY"), Endpoint: os.Getenv("OPENAI_REALTIME_ENDPOINT"), Model: os.Getenv("OPENAI_REALTIME_MODEL"), SafetyIdentifier: os.Getenv("OPENAI_SAFETY_IDENTIFIER")})
	qwenSTT, _ := qwenProvider.NewTranscriber(qwenProvider.TranscriptionConfig{APIKey: os.Getenv("DASHSCOPE_API_KEY"), Endpoint: os.Getenv("QWEN_STT_ENDPOINT"), Model: os.Getenv("QWEN_STT_MODEL")})
	openaiSTT, _ := openaiProvider.NewTranscriber(openaiProvider.TranscriptionConfig{APIKey: os.Getenv("OPENAI_API_KEY"), Endpoint: os.Getenv("OPENAI_STT_ENDPOINT"), Model: os.Getenv("OPENAI_STT_MODEL"), Delay: os.Getenv("OPENAI_STT_DELAY")})
	_ = registry.Register(qwen)
	_ = registry.Register(openai)
	// MiniMax currently supplies the LLM and TTS stages. The STT slot remains
	// explicit so a streaming ASR implementation can be selected independently.
	_ = registry.Register(&cascade.Provider{ProviderName: "minimax"})
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("DJONEHUB_VOICE_AGENT_PROVIDER")))
	sttProvider := strings.ToLower(strings.TrimSpace(os.Getenv("DJONEHUB_VOICE_AGENT_STT_PROVIDER")))
	if sttProvider == "" {
		if keys["qwen"] {
			sttProvider = "qwen"
		} else if keys["openai"] {
			sttProvider = "openai"
		}
	}
	state := voiceAgentState{Provider: provider, FallbackProvider: os.Getenv("DJONEHUB_VOICE_AGENT_FALLBACK_PROVIDER"), Voice: os.Getenv("DJONEHUB_VOICE_AGENT_VOICE"), Instructions: os.Getenv("DJONEHUB_VOICE_AGENT_INSTRUCTIONS"), STTProvider: sttProvider, FallbackSTT: os.Getenv("DJONEHUB_VOICE_AGENT_FALLBACK_STT_PROVIDER"), STTModel: os.Getenv("DJONEHUB_VOICE_AGENT_STT_MODEL"), ToolsEnabled: envBool("DJONEHUB_VOICE_AGENT_TOOLS_ENABLED"), AuditEnabled: envBool("DJONEHUB_VOICE_AGENT_AUDIT_ENABLED"), RedactPII: true, AutoAnswer: envBool("DJONEHUB_VOICE_AGENT_AUTO_ANSWER"), AutoAnswerDelayMS: envInt("DJONEHUB_VOICE_AGENT_AUTO_ANSWER_DELAY_MS", 1200), UpdatedAt: time.Now()}
	state.Enabled = provider != "" && envBool("DJONEHUB_VOICE_AGENT_ENABLED")
	controller := &voiceAgentController{registry: registry, state: state, keys: keys, transcribers: map[string]cascade.Transcriber{"qwen": qwenSTT, "openai": openaiSTT}, minimaxTextConfig: minimax.TextConfig{APIKey: os.Getenv("MINIMAX_API_KEY"), Endpoint: os.Getenv("MINIMAX_TEXT_ENDPOINT"), Model: os.Getenv("MINIMAX_TEXT_MODEL")}, minimaxSpeechConfig: minimax.SpeechConfig{APIKey: os.Getenv("MINIMAX_API_KEY"), Endpoint: os.Getenv("MINIMAX_TTS_ENDPOINT"), Model: os.Getenv("MINIMAX_TTS_MODEL"), Voice: os.Getenv("MINIMAX_TTS_VOICE")}, settingsPath: settingsPath, mediaToken: newMediaToken()}
	controller.initializeRuntime()
	controller.loadSettings()
	controller.state = normalizeVoiceAgentState(controller.state)
	if controller.state.Enabled {
		if err := controller.validateReady(controller.state); err != nil {
			controller.state.Enabled = false
			controller.state.LastError = err.Error()
		}
	}
	return controller
}

func newMediaToken() string {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		panic("create voice agent media token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return value
}

const voiceAgentSettingsVersion = 2

type voiceAgentSettings struct {
	Version           int    `json:"version"`
	Enabled           bool   `json:"enabled"`
	Provider          string `json:"provider"`
	FallbackProvider  string `json:"fallback_provider,omitempty"`
	Model             string `json:"model,omitempty"`
	Voice             string `json:"voice,omitempty"`
	Instructions      string `json:"instructions,omitempty"`
	STTProvider       string `json:"stt_provider,omitempty"`
	FallbackSTT       string `json:"fallback_stt_provider,omitempty"`
	STTModel          string `json:"stt_model,omitempty"`
	ToolsEnabled      bool   `json:"tools_enabled"`
	AuditEnabled      bool   `json:"audit_enabled"`
	RedactPII         bool   `json:"redact_pii"`
	AutoAnswer        bool   `json:"auto_answer"`
	AutoAnswerDelayMS int    `json:"auto_answer_delay_ms"`
}

func normalizeVoiceAgentState(state voiceAgentState) voiceAgentState {
	state.Provider = strings.ToLower(strings.TrimSpace(state.Provider))
	state.FallbackProvider = strings.ToLower(strings.TrimSpace(state.FallbackProvider))
	state.STTProvider = strings.ToLower(strings.TrimSpace(state.STTProvider))
	state.FallbackSTT = strings.ToLower(strings.TrimSpace(state.FallbackSTT))
	state.Model = strings.TrimSpace(state.Model)
	state.STTModel = strings.TrimSpace(state.STTModel)
	state.Voice = strings.TrimSpace(state.Voice)
	state.Voice = voiceForProvider(state.Provider, state.Voice)
	state.Instructions = strings.TrimSpace(state.Instructions)
	if state.AutoAnswerDelayMS == 0 {
		state.AutoAnswerDelayMS = 1200
	}
	if state.Revision == 0 {
		state.Revision = 1
	}
	if state.AutoAnswerDelayMS < 250 {
		state.AutoAnswerDelayMS = 250
	}
	if state.AutoAnswerDelayMS > 30000 {
		state.AutoAnswerDelayMS = 30000
	}
	return state
}

var openAIRealtimeVoices = map[string]struct{}{
	"alloy": {}, "ash": {}, "ballad": {}, "coral": {}, "echo": {},
	"sage": {}, "shimmer": {}, "verse": {}, "marin": {}, "cedar": {},
}

func voiceForProvider(provider, voice string) string {
	voice = strings.TrimSpace(voice)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		candidate := strings.ToLower(voice)
		if _, ok := openAIRealtimeVoices[candidate]; ok {
			return candidate
		}
		return "marin"
	case "qwen":
		if voice == "" {
			return "Cherry"
		}
		if _, isOpenAI := openAIRealtimeVoices[strings.ToLower(voice)]; isOpenAI {
			return "Cherry"
		}
	}
	return voice
}

func (c *voiceAgentController) loadSettings() {
	if c.settingsPath == "" {
		return
	}
	data, err := os.ReadFile(c.settingsPath)
	if err != nil {
		return
	}
	var settings voiceAgentSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		c.state.LastError = "load voice agent profile: " + err.Error()
		return
	}
	c.state.Enabled = settings.Enabled
	c.state.Provider = settings.Provider
	c.state.FallbackProvider = settings.FallbackProvider
	c.state.Model = settings.Model
	c.state.Voice = settings.Voice
	c.state.Instructions = settings.Instructions
	c.state.STTProvider = settings.STTProvider
	c.state.FallbackSTT = settings.FallbackSTT
	c.state.STTModel = settings.STTModel
	c.state.ToolsEnabled = settings.ToolsEnabled
	c.state.AuditEnabled = settings.AuditEnabled
	if settings.Version >= 2 {
		c.state.RedactPII = settings.RedactPII
	}
	c.state.AutoAnswer = settings.AutoAnswer
	c.state.AutoAnswerDelayMS = settings.AutoAnswerDelayMS
	c.state.UpdatedAt = time.Now()
}

func settingsFromState(state voiceAgentState) voiceAgentSettings {
	return voiceAgentSettings{Version: voiceAgentSettingsVersion, Enabled: state.Enabled, Provider: state.Provider, FallbackProvider: state.FallbackProvider, Model: state.Model, Voice: state.Voice, Instructions: state.Instructions, STTProvider: state.STTProvider, FallbackSTT: state.FallbackSTT, STTModel: state.STTModel, ToolsEnabled: state.ToolsEnabled, AuditEnabled: state.AuditEnabled, RedactPII: state.RedactPII, AutoAnswer: state.AutoAnswer, AutoAnswerDelayMS: state.AutoAnswerDelayMS}
}

func (c *voiceAgentController) saveSettings(state voiceAgentState) error {
	if c.settingsPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.settingsPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settingsFromState(state), "", "  ")
	if err != nil {
		return err
	}
	temporary := c.settingsPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, c.settingsPath)
}

func (c *voiceAgentController) validateReady(state voiceAgentState) error {
	if _, ok := c.registry.Get(state.Provider); !ok {
		return fmt.Errorf("voice agent provider %q is unavailable", state.Provider)
	}
	if !c.keys[state.Provider] {
		return fmt.Errorf("%s API key is not configured", state.Provider)
	}
	if state.Provider != "minimax" {
		return nil
	}
	if _, ok := c.transcribers[state.STTProvider]; !ok {
		return fmt.Errorf("streaming STT provider %q is unavailable", state.STTProvider)
	}
	if !c.keys[state.STTProvider] {
		return fmt.Errorf("%s STT API key is not configured", state.STTProvider)
	}
	return nil
}

func (c *voiceAgentController) providerFor(state voiceAgentState) (voiceagent.Provider, error) {
	if err := c.validateReady(state); err != nil {
		return nil, err
	}
	if state.Provider != "minimax" {
		provider, _ := c.registry.Get(state.Provider)
		return provider, nil
	}
	textConfig := c.minimaxTextConfig
	if state.Model != "" {
		textConfig.Model = state.Model
	}
	return &cascade.Provider{ProviderName: "minimax", STT: c.transcribers[state.STTProvider], LLM: minimax.NewTextModel(textConfig), TTS: minimax.NewSynthesizer(c.minimaxSpeechConfig)}, nil
}

func (a *app) ensureVoiceAgent() *voiceAgentController {
	a.voiceAgentOnce.Do(func() {
		path := a.voiceAgentSettingsPath
		if path == "" {
			if directory, err := os.UserConfigDir(); err == nil {
				path = filepath.Join(directory, "DJOneHub", "voice-agent.json")
			}
		}
		a.voiceAgent = newVoiceAgentController(path)
	})
	return a.voiceAgent
}

func (c *voiceAgentController) snapshot() voiceAgentState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (a *app) voiceAgentStatus(w http.ResponseWriter, _ *http.Request) {
	controller := a.ensureVoiceAgent()
	state := controller.snapshot()
	providers := make([]map[string]any, 0)
	for _, name := range controller.registry.Names() {
		provider, _ := controller.registry.Get(name)
		candidate := state
		candidate.Provider = name
		configured := controller.validateReady(candidate) == nil
		reason := ""
		if err := controller.validateReady(candidate); err != nil {
			reason = err.Error()
		}
		providers = append(providers, map[string]any{"name": name, "mode": provider.Mode(), "configured": configured, "reason": reason})
	}
	sttProviders := make([]map[string]any, 0, len(controller.transcribers))
	for name := range controller.transcribers {
		sttProviders = append(sttProviders, map[string]any{"name": name, "configured": controller.keys[name]})
	}
	controller.runtime.mu.Lock()
	health := make(map[string]voiceAgentProviderHealth, len(controller.runtime.providerHealth))
	for name, item := range controller.runtime.providerHealth {
		health[name] = item
	}
	pendingCount := len(controller.runtime.pendingTools)
	controller.runtime.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "media_mode": state.MediaMode(), "providers": providers, "stt_providers": sttProviders, "provider_health": health, "pending_tools": pendingCount, "audio_format": voiceagent.TelephonePCM})
}

func (a *app) voiceAgentConfigure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled           bool   `json:"enabled"`
		Provider          string `json:"provider"`
		FallbackProvider  string `json:"fallback_provider"`
		Model             string `json:"model"`
		Voice             string `json:"voice"`
		Instructions      string `json:"instructions"`
		STTProvider       string `json:"stt_provider"`
		FallbackSTT       string `json:"fallback_stt_provider"`
		STTModel          string `json:"stt_model"`
		ToolsEnabled      bool   `json:"tools_enabled"`
		AuditEnabled      bool   `json:"audit_enabled"`
		RedactPII         bool   `json:"redact_pii"`
		AutoAnswer        bool   `json:"auto_answer"`
		AutoAnswerDelayMS int    `json:"auto_answer_delay_ms"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	controller := a.ensureVoiceAgent()
	body.Provider = strings.ToLower(strings.TrimSpace(body.Provider))
	candidate := normalizeVoiceAgentState(voiceAgentState{Enabled: body.Enabled, Provider: body.Provider, FallbackProvider: body.FallbackProvider, Model: strings.TrimSpace(body.Model), Voice: strings.TrimSpace(body.Voice), Instructions: strings.TrimSpace(body.Instructions), STTProvider: strings.ToLower(strings.TrimSpace(body.STTProvider)), FallbackSTT: body.FallbackSTT, STTModel: strings.TrimSpace(body.STTModel), ToolsEnabled: body.ToolsEnabled, AuditEnabled: body.AuditEnabled, RedactPII: body.RedactPII, AutoAnswer: body.AutoAnswer, AutoAnswerDelayMS: body.AutoAnswerDelayMS, UpdatedAt: time.Now()})
	if _, ok := controller.registry.Get(candidate.Provider); candidate.Provider != "" && !ok {
		writeError(w, http.StatusBadRequest, "unknown voice agent provider")
		return
	}
	if _, ok := controller.registry.Get(candidate.FallbackProvider); candidate.FallbackProvider != "" && !ok {
		writeError(w, http.StatusBadRequest, "unknown fallback voice agent provider")
		return
	}
	if candidate.FallbackSTT != "" {
		if _, ok := controller.transcribers[candidate.FallbackSTT]; !ok {
			writeError(w, http.StatusBadRequest, "unknown fallback STT provider")
			return
		}
	}
	if candidate.Enabled {
		if err := controller.validateReady(candidate); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if err := controller.saveSettings(candidate); err != nil {
		writeError(w, http.StatusInternalServerError, "persist voice agent profile: "+err.Error())
		return
	}
	controller.mu.Lock()
	candidate.Revision = controller.state.Revision + 1
	candidate.Connected = controller.state.Connected
	candidate.CallID = controller.state.CallID
	candidate.ActiveProvider = controller.state.ActiveProvider
	candidate.ReconnectCount = controller.state.ReconnectCount
	controller.state = candidate
	state := controller.state
	controller.mu.Unlock()
	controller.discardPreparedSession("")
	a.prepareVoiceAgentForCurrentCall(state, true)
	writeJSON(w, http.StatusOK, state)
}

var voiceAgentUpgrader = websocket.Upgrader{
	ReadBufferSize: 4096, WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		parsed, err := url.Parse(origin)
		return err == nil && strings.EqualFold(parsed.Host, r.Host)
	},
}

func voiceAgentSessionConfig(callID string, state voiceAgentState) voiceagent.SessionConfig {
	tools := []voiceagent.Tool(nil)
	instructions := state.Instructions
	if state.ToolsEnabled {
		tools = voiceAgentTools()
		instructions = strings.TrimSpace(instructions + `

结束通话规则：当对方明确道别、要求结束通话，或当前任务已经完成且适合结束时，必须在当前轮立即调用 hang_up_call，不要让对方再确认一次。该工具会等待你的最后一句语音实际播放完毕后才挂断；工具返回成功后，请立即说一句简短、自然的告别语，不要再询问是否挂断。`)
	}
	return voiceagent.SessionConfig{
		ID: callID, Model: state.Model, TranscriptionModel: state.STTModel,
		Voice: state.Voice, Instructions: instructions,
		OpeningPrompt: "电话已经接通。请立即主动、简短而自然地问候对方，并依据系统角色与通话指令开始会话。不要提及这条触发指令。",
		Language:      "zh",
		Tools:         tools,
	}
}

func (c *voiceAgentController) openVoiceAgentSession(ctx context.Context, state voiceAgentState, config voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
	if c.openSessionOverride != nil {
		return c.openSessionOverride(ctx, state, config)
	}
	return c.openSession(ctx, state, config)
}

func closePreparedVoiceAgentSession(prepared *preparedVoiceAgentSession) {
	if prepared == nil {
		return
	}
	prepared.cancel()
	if prepared.session != nil {
		_ = prepared.session.Close()
	}
}

func (c *voiceAgentController) prepareSession(callID string, state voiceAgentState, config voiceagent.SessionConfig) {
	if callID == "" || !state.Enabled {
		c.discardPreparedSession("")
		return
	}
	c.preparedMu.Lock()
	if current := c.prepared; current != nil && current.callID == callID && current.revision == state.Revision {
		c.preparedMu.Unlock()
		return
	}
	previous := c.prepared
	ctx, cancel := context.WithCancel(context.Background())
	prepared := &preparedVoiceAgentSession{callID: callID, revision: state.Revision, ready: make(chan struct{}), cancel: cancel}
	c.prepared = prepared
	c.preparedMu.Unlock()
	closePreparedVoiceAgentSession(previous)

	go func() {
		session, profile, err := c.openVoiceAgentSession(ctx, state, config)
		c.preparedMu.Lock()
		if c.prepared != prepared {
			c.preparedMu.Unlock()
			cancel()
			if session != nil {
				_ = session.Close()
			}
			return
		}
		prepared.session = session
		prepared.profile = profile
		prepared.err = err
		close(prepared.ready)
		c.preparedMu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			c.setError(fmt.Errorf("prepare voice agent session: %w", err))
		}
	}()
}

func (c *voiceAgentController) discardPreparedSession(callID string) {
	c.preparedMu.Lock()
	prepared := c.prepared
	if prepared == nil || (callID != "" && prepared.callID != callID) {
		c.preparedMu.Unlock()
		return
	}
	c.prepared = nil
	c.preparedMu.Unlock()
	closePreparedVoiceAgentSession(prepared)
}

func (c *voiceAgentController) takePreparedSession(ctx context.Context, callID string, revision uint64) (voiceagent.Session, voiceAgentState, context.CancelFunc, bool, error) {
	c.preparedMu.Lock()
	prepared := c.prepared
	if prepared == nil || prepared.callID != callID || prepared.revision != revision {
		c.preparedMu.Unlock()
		return nil, voiceAgentState{}, nil, false, nil
	}
	ready := prepared.ready
	c.preparedMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, voiceAgentState{}, nil, true, ctx.Err()
	case <-ready:
	}
	c.preparedMu.Lock()
	if c.prepared != prepared {
		c.preparedMu.Unlock()
		return nil, voiceAgentState{}, nil, false, nil
	}
	c.prepared = nil
	session, profile, err := prepared.session, prepared.profile, prepared.err
	prepared.session = nil
	c.preparedMu.Unlock()
	if err != nil {
		prepared.cancel()
		return nil, voiceAgentState{}, nil, true, err
	}
	return session, profile, prepared.cancel, true, nil
}

func (a *app) prepareVoiceAgentForCurrentCall(state voiceAgentState, force bool) {
	controller := a.ensureVoiceAgent()
	if a.demo {
		controller.discardPreparedSession("")
		return
	}
	if state.Connected && !force {
		controller.discardPreparedSession("")
		return
	}
	a.callMu.RLock()
	callID := ""
	if a.activeCall != nil {
		callID = a.activeCall.ID
	}
	a.callMu.RUnlock()
	if callID == "" || !state.Enabled {
		controller.discardPreparedSession("")
		return
	}
	controller.prepareSession(callID, state, voiceAgentSessionConfig(callID, state))
}

func (a *app) syncVoiceAgentPreparation() {
	a.prepareVoiceAgentForCurrentCall(a.ensureVoiceAgent().snapshot(), false)
}

func startVoiceAgentSession(ctx context.Context, session voiceagent.Session) error {
	starter, ok := session.(voiceagent.SessionStarter)
	if !ok {
		return nil
	}
	return starter.Start(ctx)
}

func (a *app) voiceAgentMedia(w http.ResponseWriter, r *http.Request) {
	controller := a.ensureVoiceAgent()
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(controller.mediaToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid voice agent media token")
		return
	}
	if !controller.sessionMu.TryLock() {
		writeError(w, http.StatusConflict, "a voice agent media session is already active")
		return
	}
	defer controller.sessionMu.Unlock()
	state := controller.snapshot()
	if !state.Enabled {
		writeError(w, http.StatusConflict, "voice agent mode is disabled")
		return
	}
	a.callMu.RLock()
	active := a.activeCall
	callID := ""
	if active != nil && active.State == "active" {
		callID = active.ID
	}
	a.callMu.RUnlock()
	if callID == "" {
		writeError(w, http.StatusConflict, "there is no active call")
		return
	}
	session, activeProfile, preparedCancel, foundPrepared, err := controller.takePreparedSession(r.Context(), callID, state.Revision)
	if err != nil && foundPrepared && !errors.Is(err, context.Canceled) {
		log.Printf("prepared voice agent session was unavailable; reconnecting: %v", err)
		session = nil
	}
	if session == nil && r.Context().Err() == nil {
		session, activeProfile, err = controller.openVoiceAgentSession(r.Context(), state, voiceAgentSessionConfig(callID, state))
	}
	if err != nil {
		controller.setError(err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	conn, err := voiceAgentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = session.Close()
		if preparedCancel != nil {
			preparedCancel()
		}
		return
	}
	defer conn.Close()
	defer func() {
		if session != nil {
			_ = session.Close()
		}
		if preparedCancel != nil {
			preparedCancel()
		}
	}()
	if err := startVoiceAgentSession(r.Context(), session); err != nil && preparedCancel != nil && r.Context().Err() == nil {
		// An idle preconnected session may have expired while the phone kept
		// ringing. Reconnect inside the existing local media bridge instead of
		// forcing the audio host through its reconnect backoff.
		_ = session.Close()
		preparedCancel()
		preparedCancel = nil
		session, activeProfile, err = controller.openVoiceAgentSession(r.Context(), state, voiceAgentSessionConfig(callID, state))
		if err == nil {
			err = startVoiceAgentSession(r.Context(), session)
		}
		if err != nil {
			controller.setError(fmt.Errorf("restart expired voice agent greeting: %w", err))
			return
		}
	} else if err != nil {
		controller.setError(fmt.Errorf("start voice agent greeting: %w", err))
		return
	}
	activeProvider := voiceAgentProviderKey(activeProfile)
	controller.setConnected(callID, activeProvider, true, "")
	defer controller.setConnected("", "", false, "")
	defer controller.cancelPendingHangup(callID)
	readFailures := make(chan error, 1)
	mediaEvents := make(chan string, 8)
	go func() {
		for {
			kind, payload, err := conn.ReadMessage()
			if err != nil {
				readFailures <- err
				return
			}
			if kind == websocket.TextMessage {
				var message struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(payload, &message) == nil && message.Type != "" {
					select {
					case mediaEvents <- message.Type:
					case <-r.Context().Done():
						return
					}
				}
				continue
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			if err := session.SendAudio(r.Context(), payload); err != nil {
				readFailures <- err
				return
			}
		}
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case err := <-readFailures:
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				controller.setError(err)
			}
			return
		case eventType := <-mediaEvents:
			if eventType == "playout.done" {
				a.completeVoiceAgentHangup(controller, callID)
			}
		case event, open := <-session.Events():
			if !open {
				if r.Context().Err() == nil {
					controller.markProviderFailure(activeProvider, fmt.Errorf("provider session closed unexpectedly"))
				}
				return
			}
			a.observeVoiceAgentHangupEvent(controller, callID, event)
			controller.recordEvent(callID, event)
			if event.Err != nil {
				controller.setError(event.Err)
				controller.markProviderFailure(activeProvider, event.Err)
			}
			if event.Type == voiceagent.EventSessionReady {
				controller.markProviderReady(activeProvider)
			}
			if event.Type == voiceagent.EventClosed && r.Context().Err() == nil {
				controller.markProviderFailure(activeProvider, fmt.Errorf("provider session closed unexpectedly"))
			}
			if event.Type == voiceagent.EventToolCall && event.ToolCall != nil {
				a.handleVoiceAgentTool(controller, session, callID, event.ToolCall)
			}
			if event.Type == voiceagent.EventAudio {
				if err := conn.WriteMessage(websocket.BinaryMessage, event.Audio); err != nil {
					controller.setError(err)
					return
				}
				continue
			}
			metadata := map[string]any{"type": event.Type, "text": event.Text, "provider": event.Provider}
			if event.Err != nil {
				metadata["error"] = event.Err.Error()
			}
			payload, _ := json.Marshal(metadata)
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				controller.setError(err)
				return
			}
			if event.Type == voiceagent.EventClosed {
				return
			}
		}
	}
}

func (c *voiceAgentController) setConnected(callID, activeProvider string, connected bool, lastError string) {
	reconnected := false
	if connected {
		c.runtime.mu.Lock()
		reconnected = c.runtime.lastConnectedCall == callID
		c.runtime.lastConnectedCall = callID
		c.runtime.mu.Unlock()
	}
	c.mu.Lock()
	if connected {
		if reconnected {
			c.state.ReconnectCount++
		} else {
			c.state.ReconnectCount = 0
		}
	}
	c.state.Connected = connected
	c.state.CallID = callID
	c.state.ActiveProvider = activeProvider
	if connected || lastError != "" {
		c.state.LastError = lastError
	}
	c.state.UpdatedAt = time.Now()
	c.mu.Unlock()
	if connected {
		// The media handler owns the active session. Close a duplicate prepared
		// session if a call-state poll raced with the handoff.
		c.discardPreparedSession(callID)
	}
}
func (c *voiceAgentController) setError(err error) {
	if err == nil {
		return
	}
	log.Printf("voice agent: %v", err)
	c.mu.Lock()
	c.state.LastError = err.Error()
	c.state.UpdatedAt = time.Now()
	c.mu.Unlock()
}

func (a *app) scheduleVoiceAgentAutoAnswer(previousState, currentState string) {
	if previousState == currentState || (currentState != "incoming" && currentState != "waiting") {
		return
	}
	controller := a.ensureVoiceAgent()
	state := controller.snapshot()
	if !state.Enabled || !state.AutoAnswer {
		return
	}
	a.callMu.RLock()
	callID := ""
	if a.activeCall != nil {
		callID = a.activeCall.ID
	}
	a.callMu.RUnlock()
	if callID == "" {
		return
	}
	delay := time.Duration(state.AutoAnswerDelayMS) * time.Millisecond
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		latest := controller.snapshot()
		if !latest.Enabled || !latest.AutoAnswer {
			return
		}
		a.callMu.RLock()
		stillRinging := a.activeCall != nil && a.activeCall.ID == callID && (a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
		a.callMu.RUnlock()
		if !stillRinging {
			return
		}
		if _, err := a.answerCurrentCall(); err != nil {
			controller.setError(fmt.Errorf("voice agent auto-answer: %w", err))
		}
	}()
}
