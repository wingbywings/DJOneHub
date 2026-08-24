package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

const voiceAgentCallRecordLimit = 100

type voiceAgentCallMessage struct {
	Role string    `json:"role"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

type voiceAgentCallRecord struct {
	CallID        string                  `json:"call_id"`
	Number        string                  `json:"number,omitempty"`
	Direction     string                  `json:"direction"`
	Provider      string                  `json:"provider,omitempty"`
	StartedAt     time.Time               `json:"started_at"`
	EndedAt       *time.Time              `json:"ended_at,omitempty"`
	Messages      []voiceAgentCallMessage `json:"messages"`
	RecordingPath string                  `json:"recording_path,omitempty"`
}

type voiceAgentCallView struct {
	CallID             string                  `json:"call_id"`
	Number             string                  `json:"number,omitempty"`
	Direction          string                  `json:"direction"`
	Provider           string                  `json:"provider,omitempty"`
	StartedAt          time.Time               `json:"started_at"`
	EndedAt            *time.Time              `json:"ended_at,omitempty"`
	Messages           []voiceAgentCallMessage `json:"messages"`
	RecordingAvailable bool                    `json:"recording_available"`
	RecordingFilename  string                  `json:"recording_filename,omitempty"`
}

type voiceAgentCallRecordFile struct {
	Version int                    `json:"version"`
	Calls   []voiceAgentCallRecord `json:"calls"`
}

func (c *voiceAgentController) loadCallRecords() {
	if c.runtime == nil || c.runtime.callRecordsPath == "" {
		return
	}
	data, err := os.ReadFile(c.runtime.callRecordsPath)
	if err != nil {
		return
	}
	var stored voiceAgentCallRecordFile
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	if len(stored.Calls) > voiceAgentCallRecordLimit {
		stored.Calls = stored.Calls[len(stored.Calls)-voiceAgentCallRecordLimit:]
	}
	c.runtime.mu.Lock()
	c.runtime.callRecords = stored.Calls
	c.runtime.mu.Unlock()
}

func writeVoiceAgentCallRecords(path string, calls []voiceAgentCallRecord) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(voiceAgentCallRecordFile{Version: 1, Calls: calls}, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func cloneVoiceAgentCallRecords(calls []voiceAgentCallRecord) []voiceAgentCallRecord {
	cloned := make([]voiceAgentCallRecord, len(calls))
	for index, call := range calls {
		cloned[index] = call
		cloned[index].Messages = append([]voiceAgentCallMessage(nil), call.Messages...)
	}
	return cloned
}

func (c *voiceAgentController) persistCallRecords() {
	if c.runtime == nil {
		return
	}
	c.runtime.callFileMu.Lock()
	defer c.runtime.callFileMu.Unlock()
	c.runtime.mu.Lock()
	path := c.runtime.callRecordsPath
	calls := cloneVoiceAgentCallRecords(c.runtime.callRecords)
	c.runtime.mu.Unlock()
	if path == "" {
		return
	}
	if err := writeVoiceAgentCallRecords(path, calls); err != nil {
		// The audit log remains the operational fallback if the call archive
		// cannot be rewritten. Do not interrupt the live media session.
		log.Printf("persist voice agent call record: %v", err)
	}
}

func (c *voiceAgentController) updateCallRecord(callID string, update func(*voiceAgentCallRecord) bool) {
	if c.runtime == nil || strings.TrimSpace(callID) == "" {
		return
	}
	c.runtime.mu.Lock()
	changed := false
	for index := range c.runtime.callRecords {
		if c.runtime.callRecords[index].CallID == callID {
			changed = update(&c.runtime.callRecords[index])
			break
		}
	}
	c.runtime.mu.Unlock()
	if !changed {
		return
	}
	c.persistCallRecords()
}

func (c *voiceAgentController) startCallRecord(call callRecord, provider string) {
	if c.runtime == nil || call.ID == "" {
		return
	}
	c.runtime.mu.Lock()
	for index := range c.runtime.callRecords {
		if c.runtime.callRecords[index].CallID == call.ID {
			c.runtime.mu.Unlock()
			return
		}
	}
	c.runtime.callRecords = append(c.runtime.callRecords, voiceAgentCallRecord{
		CallID: call.ID, Number: call.Number, Direction: call.Direction,
		Provider: provider, StartedAt: call.StartedAt, Messages: []voiceAgentCallMessage{},
	})
	if len(c.runtime.callRecords) > voiceAgentCallRecordLimit {
		c.runtime.callRecords = append([]voiceAgentCallRecord(nil), c.runtime.callRecords[len(c.runtime.callRecords)-voiceAgentCallRecordLimit:]...)
	}
	c.runtime.mu.Unlock()
	c.persistCallRecords()
}

func (c *voiceAgentController) finishCallRecord(callID string, endedAt time.Time) {
	c.updateCallRecord(callID, func(record *voiceAgentCallRecord) bool {
		if record.EndedAt != nil {
			return false
		}
		ended := endedAt
		record.EndedAt = &ended
		return true
	})
}

func (c *voiceAgentController) recordCallEvent(callID string, event voiceagent.Event) {
	role := ""
	switch event.Type {
	case voiceagent.EventInputTranscriptFinal:
		role = "caller"
	case voiceagent.EventOutputTranscriptFinal:
		role = "ai"
	case "recording.stopped":
		path := strings.TrimSpace(event.Text)
		if path == "" {
			return
		}
		c.updateCallRecord(callID, func(record *voiceAgentCallRecord) bool {
			if record.RecordingPath == path {
				return false
			}
			record.RecordingPath = path
			return true
		})
		return
	default:
		return
	}
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return
	}
	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	c.updateCallRecord(callID, func(record *voiceAgentCallRecord) bool {
		if event.Provider != "" {
			record.Provider = event.Provider
		}
		if count := len(record.Messages); count > 0 {
			last := record.Messages[count-1]
			if last.Role == role && last.Text == text {
				return false
			}
		}
		record.Messages = append(record.Messages, voiceAgentCallMessage{Role: role, Text: text, At: at})
		return true
	})
}

func (a *app) voiceAgentCalls(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if parsed, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && parsed > 0 {
		limit = min(parsed, voiceAgentCallRecordLimit)
	}
	controller := a.ensureVoiceAgent()
	controller.runtime.mu.Lock()
	records := cloneVoiceAgentCallRecords(controller.runtime.callRecords)
	controller.runtime.mu.Unlock()
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	views := make([]voiceAgentCallView, 0, len(records))
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		view := voiceAgentCallView{
			CallID: record.CallID, Number: record.Number, Direction: record.Direction,
			Provider: record.Provider, StartedAt: record.StartedAt, EndedAt: record.EndedAt,
			Messages: append([]voiceAgentCallMessage(nil), record.Messages...),
		}
		if resolved, err := a.resolveVoiceAgentRecording(record.RecordingPath); err == nil {
			if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() {
				view.RecordingAvailable = true
				view.RecordingFilename = filepath.Base(resolved)
			}
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": views, "count": len(views)})
}

func (a *app) voiceAgentCallRecording(w http.ResponseWriter, r *http.Request) {
	callID := strings.TrimSpace(r.PathValue("callID"))
	if callID == "" {
		writeError(w, http.StatusBadRequest, "invalid call ID")
		return
	}
	controller := a.ensureVoiceAgent()
	controller.runtime.mu.Lock()
	path := ""
	for _, record := range controller.runtime.callRecords {
		if record.CallID == callID {
			path = record.RecordingPath
			break
		}
	}
	controller.runtime.mu.Unlock()
	resolved, err := a.resolveVoiceAgentRecording(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	file, err := os.Open(resolved)
	if err != nil {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, filepath.Base(resolved)))
	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), file)
}
