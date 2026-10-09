package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/remotemedia/opus"
)

type remoteCallController struct {
	mu           sync.RWMutex
	config       callcontrol.RemoteConfig
	settingsPath string
	loadError    string
}

func newRemoteCallController(path string) *remoteCallController {
	defaults, _ := callcontrol.NormalizeRemoteConfig(callcontrol.RemoteConfig{})
	c := &remoteCallController{settingsPath: path, config: defaults}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c
	}
	if err != nil {
		c.loadError = "load remote call settings: " + err.Error()
		return c
	}
	var stored struct {
		Version int                      `json:"version"`
		Config  callcontrol.RemoteConfig `json:"config"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		c.loadError = "load remote call settings: " + err.Error()
		return c
	}
	if stored.Version != 1 {
		c.loadError = "unsupported remote call settings version"
		return c
	}
	config, err := callcontrol.NormalizeRemoteConfig(stored.Config)
	if err != nil {
		c.loadError = "load remote call settings: " + err.Error()
		return c
	}
	c.config = config
	return c
}

func (c *remoteCallController) snapshot() (callcontrol.RemoteConfig, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config, c.loadError
}

// Caller holds app.callRouteMu to serialize updates with AI configuration and
// new-call route selection. Publish only after the atomic file replacement.
func (c *remoteCallController) save(config callcontrol.RemoteConfig) error {
	if c.settingsPath != "" {
		if err := os.MkdirAll(filepath.Dir(c.settingsPath), 0o700); err != nil {
			return err
		}
		data, err := json.MarshalIndent(struct {
			Version int                      `json:"version"`
			Config  callcontrol.RemoteConfig `json:"config"`
		}{1, config}, "", "  ")
		if err != nil {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(c.settingsPath), ".remote-calls-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err = file.Write(data); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Rename(file.Name(), c.settingsPath); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.config, c.loadError = config, ""
	c.mu.Unlock()
	return nil
}

func (a *app) ensureRemoteCalls() *remoteCallController {
	a.remoteCallOnce.Do(func() {
		path := a.remoteCallSettingsPath
		if path == "" && !a.demo {
			if directory, err := os.UserConfigDir(); err == nil {
				path = filepath.Join(directory, "DJOneHub", "remote-calls.json")
			}
		}
		a.remoteCalls = newRemoteCallController(path)
	})
	return a.remoteCalls
}

func (a *app) remoteCallConfig(w http.ResponseWriter, _ *http.Request) {
	config, _ := a.ensureRemoteCalls().snapshot()
	writeJSON(w, http.StatusOK, config)
}

func (a *app) remoteCallConfigure(w http.ResponseWriter, r *http.Request) {
	var config callcontrol.RemoteConfig
	if !decodeJSON(w, r, &config) {
		return
	}
	config, err := callcontrol.NormalizeRemoteConfig(config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.callRouteMu.Lock()
	defer a.callRouteMu.Unlock()
	controller := a.ensureRemoteCalls()
	previous, loadError := controller.snapshot()
	config.Revision = previous.Revision
	if config == previous && loadError == "" {
		writeJSON(w, http.StatusOK, config)
		return
	}
	if a.hasActiveCall() {
		writeCallRouteLocked(w)
		return
	}
	config.Revision++
	if err := controller.save(config); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("persist remote call settings: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func writeCallRouteLocked(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"code": "route_locked_by_call", "error": "当前模块有来电或通话，请在通话结束后修改接听方式",
	})
}

func (a *app) remoteCallStatus(w http.ResponseWriter, _ *http.Request) {
	a.callRouteMu.Lock()
	defer a.callRouteMu.Unlock()
	config, loadError := a.ensureRemoteCalls().snapshot()
	agent := a.ensureVoiceAgent().snapshot()
	route := callcontrol.SelectRoute(agent.Enabled, agent.Revision, config)
	reason := "disabled"
	if loadError != "" {
		reason = "configuration_error"
	}
	if config.Enabled {
		reason = "transport_not_implemented"
	}
	if agent.Enabled {
		reason = "ai_route_selected"
	}
	a.callMu.RLock()
	var activeRoute *callcontrol.RouteSnapshot
	if a.activeCall != nil {
		copied := a.activeCall.Route
		activeRoute = &copied
	}
	a.callMu.RUnlock()
	connectionState := "disabled"
	if a.remoteConnectionState != nil {
		connectionState = a.remoteConnectionState()
	}
	configured := connectionState != "disabled"
	if configured && config.Enabled && !agent.Enabled {
		reason = "online_only_not_production"
	}
	var sessionState map[string]any
	a.remoteSessionMu.Lock()
	if a.remoteSession != nil {
		snapshot := a.remoteSession.session.Snapshot()
		sessionState = map[string]any{
			"call_uuid": snapshot.CallUUID, "generation": snapshot.Generation,
			"state": snapshot.State, "revision": snapshot.Revision, "end_reason": snapshot.EndReason,
			"cleanup_failed": snapshot.CleanupError != "",
		}
		if a.remoteSession.peer != nil {
			sessionState["media"] = a.remoteSession.peer.SignalingState()
		}
	}
	a.remoteSessionMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"online_transport": map[string]any{"configured": configured, "state": connectionState},
		"config":           config, "effective_route": route.Mode, "active_route": activeRoute,
		"available": false, "reason": reason, "last_error": loadError,
		"session":      sessionState,
		"codec":        map[string]any{"available": opus.Available(), "version": opus.Version()},
		"capabilities": map[string]bool{"signaling": configured, "voip_push": false, "remote_media": configured && opus.Available()},
	})
}

// An empty snapshot is retained for in-memory legacy callers/tests. All newly
// observed calls carry an explicit immutable route. Caller holds callMu.
func callUsesAgent(call *callRecord, agent voiceAgentState) bool {
	return call != nil && agent.Enabled && (call.Route.Mode == callcontrol.Agent || call.Route.Mode == "")
}
