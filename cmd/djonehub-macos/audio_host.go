package main

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

type audioHostState struct {
	Registered      bool      `json:"registered"`
	Running         bool      `json:"running"`
	Muted           bool      `json:"muted"`
	Recording       bool      `json:"recording"`
	WantMuted       bool      `json:"want_muted"`
	WantRecording   bool      `json:"want_recording"`
	RecordingPath   string    `json:"recording_path,omitempty"`
	Error           string    `json:"error,omitempty"`
	LastSeen        time.Time `json:"last_seen,omitempty"`
	recordingCallID string
}

type audioHostConfigResponse struct {
	DeviceID      string `json:"device_id"`
	VendorID      uint16 `json:"vendor_id"`
	ProductID     uint16 `json:"product_id"`
	LocationID    uint32 `json:"location_id"`
	RouteReady    bool   `json:"route_ready"`
	RouteError    string `json:"route_error,omitempty"`
	CallID        string `json:"call_id,omitempty"`
	CallActive    bool   `json:"call_active"`
	Muted         bool   `json:"muted"`
	Recording     bool   `json:"recording"`
	MediaMode     string `json:"media_mode"`
	AgentProvider string `json:"agent_provider,omitempty"`
	AgentRevision uint64 `json:"agent_revision,omitempty"`
	AgentMediaURL string `json:"agent_media_url,omitempty"`
}

func (a *app) audioHostSnapshot() audioHostState {
	a.audioHostMu.RLock()
	state := a.audioHost
	a.audioHostMu.RUnlock()
	if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 5*time.Second {
		state.Registered = false
		state.Running = false
	}
	return state
}

func (a *app) audioHostRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled       bool   `json:"enabled"`
		Running       bool   `json:"running"`
		Muted         bool   `json:"muted"`
		Recording     bool   `json:"recording"`
		RecordingPath string `json:"recording_path"`
		Error         string `json:"error"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.callMu.RLock()
	activeCallID := ""
	if a.activeCall != nil {
		activeCallID = a.activeCall.ID
	}
	a.callMu.RUnlock()
	a.audioHostMu.Lock()
	wasRecording := a.audioHost.Recording
	previousPath := a.audioHost.RecordingPath
	recordingCallID := a.audioHost.recordingCallID
	a.audioHost.Registered = body.Enabled
	a.audioHost.Running = body.Running
	a.audioHost.Muted = body.Muted
	a.audioHost.Recording = body.Recording
	a.audioHost.RecordingPath = body.RecordingPath
	a.audioHost.Error = body.Error
	a.audioHost.LastSeen = time.Now()
	if body.Recording {
		if !wasRecording || recordingCallID == "" {
			recordingCallID = activeCallID
		}
		a.audioHost.recordingCallID = recordingCallID
	} else if wasRecording {
		a.audioHost.recordingCallID = ""
	}
	state := a.audioHost
	a.audioHostMu.Unlock()
	if wasRecording != body.Recording || (body.Recording && previousPath != body.RecordingPath) {
		callID := activeCallID
		eventType := voiceagent.EventType("recording.stopped")
		text := previousPath
		if body.Recording {
			eventType = "recording.started"
			text = body.RecordingPath
			callID = recordingCallID
		} else if recordingCallID != "" {
			callID = recordingCallID
		}
		a.ensureVoiceAgent().recordEvent(callID, voiceagent.Event{Type: eventType, Text: text, At: time.Now()})
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *app) audioHostConfig(w http.ResponseWriter, _ *http.Request) {
	a.operationMu.RLock()
	defer a.operationMu.RUnlock()
	if a.usbLocator == nil || a.usbLocator.LocationID == 0 {
		writeError(w, http.StatusConflict, "当前模块缺少可验证的 CoreAudio Location ID")
		return
	}
	a.callMu.RLock()
	callID := ""
	callActive := false
	if a.activeCall != nil {
		callID = a.activeCall.ID
		callActive = a.activeCall.State == "active"
	}
	a.callMu.RUnlock()
	a.moduleVoiceMu.Lock()
	routeReady := a.moduleVoiceReady
	routeError := a.moduleVoiceErr
	a.moduleVoiceMu.Unlock()
	a.audioHostMu.RLock()
	wantMuted := a.audioHost.WantMuted
	wantRecording := a.audioHost.WantRecording
	a.audioHostMu.RUnlock()
	agent := a.ensureVoiceAgent().snapshot()
	agentMediaURL := ""
	if agent.Enabled {
		agentMediaURL = "/api/calls/audio/agent/media?token=" + url.QueryEscape(a.ensureVoiceAgent().mediaToken)
		if a.deviceID != "" {
			agentMediaURL = "/api/devices/" + url.PathEscape(a.deviceID) + "/calls/audio/agent/media?token=" + url.QueryEscape(a.ensureVoiceAgent().mediaToken)
		}
	}
	config := audioHostConfigResponse{
		DeviceID: a.deviceID, VendorID: a.usbLocator.VendorID, ProductID: a.usbLocator.ProductID,
		LocationID: a.usbLocator.LocationID, RouteReady: routeReady, RouteError: routeError,
		CallID: callID, CallActive: callActive, Muted: wantMuted, Recording: wantRecording,
		MediaMode: agent.MediaMode(), AgentProvider: agent.Provider, AgentMediaURL: agentMediaURL,
		AgentRevision: agent.Revision,
	}
	if err := validateAudioHostConfig(config); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (a *app) audioHostMute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Muted bool `json:"muted"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !a.callStateAllows("active") {
		writeError(w, http.StatusConflict, "当前没有已接通的通话")
		return
	}
	a.audioHostMu.Lock()
	a.audioHost.WantMuted = body.Muted
	a.audioHostMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"muted": body.Muted})
}

func (a *app) audioHostRecord(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled && !a.callStateAllows("active") {
		writeError(w, http.StatusConflict, "当前没有已接通的通话")
		return
	}
	if !body.Enabled {
		a.callMu.RLock()
		recordingRequired := a.activeCall != nil && a.activeCall.State == "active" && a.activeCall.AIHandled
		a.callMu.RUnlock()
		if recordingRequired {
			writeError(w, http.StatusConflict, "AI 接听的电话必须保留完整录音")
			return
		}
	}
	a.audioHostMu.Lock()
	a.audioHost.WantRecording = body.Enabled
	a.audioHostMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"recording": body.Enabled})
}

func (a *app) resetAudioHostIntent() {
	a.audioHostMu.Lock()
	a.audioHost.WantMuted = false
	a.audioHost.WantRecording = false
	a.audioHostMu.Unlock()
}

func validateAudioHostConfig(config audioHostConfigResponse) error {
	if config.VendorID == 0 || config.ProductID == 0 || config.LocationID == 0 {
		return errors.New("音频宿主配置缺少精确 USB 标识")
	}
	return nil
}
