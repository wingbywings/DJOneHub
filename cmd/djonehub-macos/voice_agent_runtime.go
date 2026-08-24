package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

const (
	voiceAgentAuditLimit    = 500
	voiceAgentAuditMaxBytes = 5 << 20
)

type voiceAgentAuditEvent struct {
	Sequence  uint64               `json:"sequence"`
	CallID    string               `json:"call_id,omitempty"`
	Type      voiceagent.EventType `json:"type"`
	Provider  string               `json:"provider,omitempty"`
	Text      string               `json:"text,omitempty"`
	Error     string               `json:"error,omitempty"`
	ToolCall  *voiceagent.ToolCall `json:"tool_call,omitempty"`
	Persisted bool                 `json:"persisted"`
	At        time.Time            `json:"at"`
}

type voiceAgentProviderHealth struct {
	Failures   int       `json:"failures"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

type pendingVoiceAgentTool struct {
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Summary   string          `json:"summary"`
	ExpiresAt time.Time       `json:"expires_at"`
	session   voiceagent.Session
}

type voiceAgentRuntime struct {
	mu                sync.Mutex
	auditFileMu       sync.Mutex
	sequence          uint64
	audit             []voiceAgentAuditEvent
	auditPath         string
	subscribers       map[uint64]chan voiceAgentAuditEvent
	nextSubscriber    uint64
	pendingTools      map[string]*pendingVoiceAgentTool
	providerHealth    map[string]voiceAgentProviderHealth
	lastConnectedCall string
}

func (c *voiceAgentController) initializeRuntime() {
	path := ""
	if c.settingsPath != "" {
		path = filepath.Join(filepath.Dir(c.settingsPath), "voice-agent-audit.jsonl")
	}
	c.runtime = &voiceAgentRuntime{
		auditPath:      path,
		subscribers:    make(map[uint64]chan voiceAgentAuditEvent),
		pendingTools:   make(map[string]*pendingVoiceAgentTool),
		providerHealth: make(map[string]voiceAgentProviderHealth),
	}
	c.loadAudit()
}

var (
	voiceAgentEmailPattern = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	voiceAgentPhonePattern = regexp.MustCompile(`\+?\d[\d -]{7,}\d`)
	voiceAgentIDPattern    = regexp.MustCompile(`\b[0-9A-Za-z]{16,}\b`)
)

func redactVoiceAgentText(value string) string {
	value = voiceAgentEmailPattern.ReplaceAllString(value, "[邮箱已脱敏]")
	value = voiceAgentPhonePattern.ReplaceAllStringFunc(value, func(raw string) string {
		digits := strings.NewReplacer(" ", "", "-", "").Replace(raw)
		if len(digits) <= 7 {
			return "[号码已脱敏]"
		}
		return digits[:3] + "****" + digits[len(digits)-4:]
	})
	return voiceAgentIDPattern.ReplaceAllString(value, "[标识符已脱敏]")
}

func (c *voiceAgentController) recordEvent(callID string, event voiceagent.Event) {
	if c.runtime == nil || event.Type == voiceagent.EventAudio {
		return
	}
	state := c.snapshot()
	entry := voiceAgentAuditEvent{CallID: callID, Type: event.Type, Provider: event.Provider, Text: event.Text, ToolCall: event.ToolCall, At: event.At, Persisted: state.AuditEnabled}
	if entry.At.IsZero() {
		entry.At = time.Now()
	}
	if event.Err != nil {
		entry.Error = event.Err.Error()
	}
	if state.RedactPII {
		entry.Text = redactVoiceAgentText(entry.Text)
		entry.Error = redactVoiceAgentText(entry.Error)
		if entry.ToolCall != nil {
			copy := *entry.ToolCall
			copy.Arguments = json.RawMessage(redactVoiceAgentText(string(copy.Arguments)))
			entry.ToolCall = &copy
		}
	}
	c.runtime.publish(entry, state.AuditEnabled)
}

func (r *voiceAgentRuntime) publish(entry voiceAgentAuditEvent, persist bool) {
	r.mu.Lock()
	r.sequence++
	entry.Sequence = r.sequence
	r.audit = append(r.audit, entry)
	if len(r.audit) > voiceAgentAuditLimit {
		r.audit = append([]voiceAgentAuditEvent(nil), r.audit[len(r.audit)-voiceAgentAuditLimit:]...)
	}
	for _, subscriber := range r.subscribers {
		select {
		case subscriber <- entry:
		default:
		}
	}
	path := r.auditPath
	r.mu.Unlock()
	if persist && path != "" {
		r.auditFileMu.Lock()
		_ = appendVoiceAgentAudit(path, entry)
		r.auditFileMu.Unlock()
	}
}

func appendVoiceAgentAudit(path string, entry voiceAgentAuditEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= voiceAgentAuditMaxBytes {
		_ = os.Remove(path + ".1")
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Chmod(0o600)
}

func (c *voiceAgentController) loadAudit() {
	if c.runtime == nil || c.runtime.auditPath == "" {
		return
	}
	file, err := os.Open(c.runtime.auditPath)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var entries []voiceAgentAuditEvent
	for scanner.Scan() {
		var entry voiceAgentAuditEvent
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	if len(entries) > voiceAgentAuditLimit {
		entries = entries[len(entries)-voiceAgentAuditLimit:]
	}
	c.runtime.mu.Lock()
	c.runtime.audit = entries
	for _, entry := range entries {
		if entry.Sequence > c.runtime.sequence {
			c.runtime.sequence = entry.Sequence
		}
	}
	c.runtime.mu.Unlock()
}

func (a *app) voiceAgentAudit(w http.ResponseWriter, r *http.Request) {
	controller := a.ensureVoiceAgent()
	limit := 100
	if parsed, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && parsed > 0 {
		limit = min(parsed, voiceAgentAuditLimit)
	}
	controller.runtime.mu.Lock()
	events := append([]voiceAgentAuditEvent(nil), controller.runtime.audit...)
	controller.runtime.mu.Unlock()
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

func (a *app) clearVoiceAgentAudit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "explicit confirmation is required")
		return
	}
	controller := a.ensureVoiceAgent()
	controller.runtime.mu.Lock()
	controller.runtime.audit = nil
	path := controller.runtime.auditPath
	controller.runtime.mu.Unlock()
	if path != "" {
		controller.runtime.auditFileMu.Lock()
		defer controller.runtime.auditFileMu.Unlock()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = os.Remove(path + ".1")
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}

func (a *app) voiceAgentEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is unavailable")
		return
	}
	controller := a.ensureVoiceAgent()
	controller.runtime.mu.Lock()
	controller.runtime.nextSubscriber++
	id := controller.runtime.nextSubscriber
	events := make(chan voiceAgentAuditEvent, 64)
	controller.runtime.subscribers[id] = events
	controller.runtime.mu.Unlock()
	defer func() {
		controller.runtime.mu.Lock()
		delete(controller.runtime.subscribers, id)
		controller.runtime.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case event := <-events:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: voice-agent\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func voiceAgentProviderKey(state voiceAgentState) string {
	if state.Provider == "minimax" {
		return state.Provider + ":" + state.STTProvider
	}
	return state.Provider
}

func (c *voiceAgentController) providerCandidates(state voiceAgentState) []voiceAgentState {
	result := []voiceAgentState{state}
	if state.Provider == "minimax" && state.FallbackSTT != "" && state.FallbackSTT != state.STTProvider {
		fallback := state
		fallback.STTProvider = state.FallbackSTT
		result = append(result, fallback)
	}
	if state.FallbackProvider != "" && state.FallbackProvider != state.Provider {
		fallback := state
		fallback.Provider = state.FallbackProvider
		if fallback.Provider == "minimax" && fallback.FallbackSTT != "" {
			fallback.STTProvider = fallback.FallbackSTT
		}
		result = append(result, fallback)
	}
	seen := make(map[string]bool)
	unique := result[:0]
	for _, candidate := range result {
		key := voiceAgentProviderKey(candidate)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, candidate)
	}
	now := time.Now()
	sort.SliceStable(unique, func(i, j int) bool {
		left := c.providerHealth(voiceAgentProviderKey(unique[i])).RetryAfter.After(now)
		right := c.providerHealth(voiceAgentProviderKey(unique[j])).RetryAfter.After(now)
		return !left && right
	})
	return unique
}

func (c *voiceAgentController) providerHealth(key string) voiceAgentProviderHealth {
	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	return c.runtime.providerHealth[key]
}

func (c *voiceAgentController) markProviderFailure(key string, err error) {
	if key == "" || err == nil {
		return
	}
	c.runtime.mu.Lock()
	health := c.runtime.providerHealth[key]
	health.Failures++
	delay := time.Second * time.Duration(1<<min(health.Failures-1, 5))
	health.RetryAfter = time.Now().Add(delay)
	health.LastError = redactVoiceAgentText(err.Error())
	c.runtime.providerHealth[key] = health
	c.runtime.mu.Unlock()
}

func (c *voiceAgentController) markProviderReady(key string) {
	c.runtime.mu.Lock()
	delete(c.runtime.providerHealth, key)
	c.runtime.mu.Unlock()
}

func (c *voiceAgentController) openSession(ctx context.Context, state voiceAgentState, config voiceagent.SessionConfig) (voiceagent.Session, voiceAgentState, error) {
	var failures []string
	for _, candidate := range c.providerCandidates(state) {
		candidate.Voice = voiceForProvider(candidate.Provider, candidate.Voice)
		provider, err := c.providerFor(candidate)
		if err == nil {
			config.Model = candidate.Model
			config.TranscriptionModel = candidate.STTModel
			config.Voice = candidate.Voice
			session, openErr := provider.Open(ctx, config)
			if openErr == nil {
				return session, candidate, nil
			}
			err = openErr
		}
		key := voiceAgentProviderKey(candidate)
		c.markProviderFailure(key, err)
		failures = append(failures, key+": "+err.Error())
	}
	return nil, state, fmt.Errorf("all voice providers failed: %s", strings.Join(failures, "; "))
}
