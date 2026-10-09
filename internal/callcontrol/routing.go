// Package callcontrol defines call policies independently of AI providers and
// media transports. A call's route is selected once, when the call is observed.
package callcontrol

import (
	"crypto/rand"
	"fmt"
	"strings"
)

type Route string

const (
	Local  Route = "local"
	Agent  Route = "agent"
	Remote Route = "remote"
)

type RemoteConfig struct {
	Enabled              bool   `json:"enabled"`
	TargetInstallationID string `json:"target_installation_id"`
	RingTimeoutSeconds   int    `json:"ring_timeout_seconds"`
	CallerDisplay        string `json:"caller_display"`
	Revision             uint64 `json:"revision"`
}

func NormalizeRemoteConfig(config RemoteConfig) (RemoteConfig, error) {
	config.TargetInstallationID = strings.TrimSpace(config.TargetInstallationID)
	if len(config.TargetInstallationID) > 128 {
		return RemoteConfig{}, fmt.Errorf("target_installation_id is too long")
	}
	for _, ch := range config.TargetInstallationID {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return RemoteConfig{}, fmt.Errorf("target_installation_id contains invalid characters")
		}
	}
	if config.Enabled && config.TargetInstallationID == "" {
		return RemoteConfig{}, fmt.Errorf("target_installation_id is required when remote calls are enabled")
	}
	if config.RingTimeoutSeconds == 0 {
		config.RingTimeoutSeconds = 45
	}
	if config.RingTimeoutSeconds < 10 || config.RingTimeoutSeconds > 90 {
		return RemoteConfig{}, fmt.Errorf("ring_timeout_seconds must be between 10 and 90")
	}
	if config.CallerDisplay == "" {
		config.CallerDisplay = "number"
	}
	if config.CallerDisplay != "number" && config.CallerDisplay != "module" {
		return RemoteConfig{}, fmt.Errorf("caller_display must be number or module")
	}
	if config.Revision == 0 {
		config.Revision = 1
	}
	return config, nil
}

type RouteSnapshot struct {
	Mode                 Route  `json:"mode"`
	ConfigRevision       uint64 `json:"config_revision"`
	TargetInstallationID string `json:"target_installation_id,omitempty"`
	RingTimeoutSeconds   int    `json:"ring_timeout_seconds,omitempty"`
	CallerDisplay        string `json:"caller_display,omitempty"`
}

// SelectRoute never treats provider health or remote availability as a reason
// to cross between AI and remote answering. Failures stay in the chosen route.
func SelectRoute(aiEnabled bool, aiRevision uint64, remote RemoteConfig) RouteSnapshot {
	if aiEnabled {
		return RouteSnapshot{Mode: Agent, ConfigRevision: aiRevision}
	}
	if remote.Enabled {
		return RouteSnapshot{Mode: Remote, ConfigRevision: remote.Revision,
			TargetInstallationID: remote.TargetInstallationID,
			RingTimeoutSeconds:   remote.RingTimeoutSeconds, CallerDisplay: remote.CallerDisplay}
	}
	return RouteSnapshot{Mode: Local}
}

// NewCallUUID is a random RFC 4122 v4 identifier suitable for CallKit. The old
// modem-local record ID remains separate for existing archives and clients.
func NewCallUUID() string {
	var id [16]byte
	rand.Read(id[:])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}
