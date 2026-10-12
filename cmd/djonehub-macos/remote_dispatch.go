package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/remotecall"
	"github.com/iniwex5/vohive/internal/remotehub"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"github.com/iniwex5/vohive/internal/turncred"
	"github.com/pion/webrtc/v4"
)

var remoteOnlineProfile string
var remoteICERestart bool
var remoteSignalResume bool
var remoteTURNRenew bool
var remoteAudioRecovery bool
var remoteCallControls bool

type remoteHubService struct {
	*remotehub.Manager
	resume  func(context.Context, callsignal.Message) (*callsignal.Client, error)
	refresh func(context.Context) (string, error)
	ice     func(context.Context, string) (turncred.Credentials, error)
}

// An explicit integration entry point until background-call device validation is complete.
// No HTTP request may select a profile or load Keychain credentials.
func startRemoteHub(ctx context.Context, profile, nativeFile string, incoming func(context.Context, remotehub.Connection, callsignal.Message)) (*remoteHubService, error) {
	if profile == "" {
		return nil, nil
	}
	if !callsignal.ID(profile) || len(profile) > 64 || nativeFile == "" || !opus.Available() {
		return nil, errors.New("remote online mode requires a valid profile, --native-control-file and a remoteopus build")
	}
	store, err := systemRemoteProfiles(relayclient.Options{})
	if err != nil {
		return nil, err
	}
	// Fail visibly before starting a background retry loop for an absent or
	// ambiguous local identity. Never reenroll or overwrite it automatically.
	status, err := store.Status(profile)
	if err != nil {
		return nil, err
	}
	if status.Phase != "ready" {
		return nil, errors.New("remote profile requires operator recovery")
	}
	manager := remotehub.Start(ctx, remotehub.Config{
		RecoverCalls: remoteSignalResume,
		Connect: func(ctx context.Context) (remotehub.Connection, error) {
			readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			config, err := store.ConnectionConfig(readCtx, profile)
			cancel()
			if err != nil {
				return nil, err
			}
			if config.Kind != "hub" {
				return nil, callsignal.ErrProtocol
			}
			config.Resumption = remoteSignalResume
			// Dial's handshake is bounded; the socket inherits the manager lifetime.
			client, err := callsignal.Dial(ctx, config)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		Refresh:  func(ctx context.Context) (string, error) { return store.RefreshAccess(ctx, profile) },
		Incoming: incoming,
	})
	return &remoteHubService{Manager: manager,
		resume: func(ctx context.Context, binding callsignal.Message) (*callsignal.Client, error) {
			config, err := store.ConnectionConfig(ctx, profile)
			if err != nil {
				return nil, err
			}
			config.Resumption, config.Resume = true, &binding
			return callsignal.Dial(ctx, config)
		},
		refresh: func(ctx context.Context) (string, error) { return store.RefreshAccess(ctx, profile) }, ice: func(ctx context.Context, module string) (turncred.Credentials, error) {
			return store.ICE(ctx, profile, module)
		}}, nil
}

// Called after publishing a poll. The immutable route, not the current AI
// provider readiness, decides whether this dispatch boundary is entered.
func (a *app) dispatchRemoteIncoming() {
	if a.remoteDispatch == nil || a.deviceID == "" {
		return
	}
	a.callMu.Lock()
	call := a.activeCall
	if call == nil || call.Direction != "incoming" || call.State != "incoming" || call.Route.Mode != callcontrol.Remote || call.ControlOwner != "" || call.ControlState != "" || a.remoteAttemptUUID == call.UUID {
		a.callMu.Unlock()
		return
	}
	a.ensureCallIdentityLocked()
	copy, ctx := *call, a.callContext
	a.remoteAttemptUUID = call.UUID
	a.callMu.Unlock()
	if ctx.Err() != nil || !copy.StartedAt.Add(time.Duration(copy.Route.RingTimeoutSeconds)*time.Second).After(time.Now()) {
		return
	}
	// The callback only reserves a connection; it must never block this poll.
	a.remoteDispatch(ctx, copy)
}

func (h *usbDeviceHub) dispatchRemote(a *app, ctx context.Context, call callRecord) {
	h.mu.RLock()
	manager := h.remoteHub
	h.mu.RUnlock()
	if manager == nil {
		return
	}
	started := manager.TryStart(ctx, func(ctx context.Context, connection remotehub.Connection) {
		client, ok := connection.(*callsignal.Client)
		if !ok {
			log.Printf("remote online call not started (module %s, call %s, reason signaling_unavailable)", a.deviceID, call.UUID)
			return
		}
		h.runRemoteCall(ctx, a, call, client)
	})
	if !started {
		log.Printf("remote online call not started (module %s, call %s, reason hub_offline_or_busy)", a.deviceID, call.UUID)
	}
}

func remoteErrorCode(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, callsignal.ErrConnection):
		return "signaling_unavailable"
	case errors.Is(err, callsignal.ErrProtocol):
		return "signaling_protocol_error"
	case errors.Is(err, remotecall.ErrBinding):
		return "call_binding_invalid"
	case errors.Is(err, remotecall.ErrRoute):
		return "route_mismatch"
	case errors.Is(err, remotecall.ErrState):
		return "call_state_invalid"
	default:
		return "failed"
	}
}

func (h *usbDeviceHub) runRemoteCall(ctx context.Context, a *app, call callRecord, client *callsignal.Client) {
	h.mu.RLock()
	manager := h.remoteHub
	h.mu.RUnlock()
	if manager == nil {
		return
	}
	// One attempt per UUID, so generation 1 is never reused for a retry.
	// Manager owns authorization throughout both idle and active phases.
	media := remotemedia.PeerConfig{Control: remoteSignalResume, CallUUID: call.UUID, Generation: 1}
	var iceExpiry time.Time
	if manager.ice != nil {
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		credentials, err := manager.ice(fetchCtx, a.deviceID)
		cancel()
		if err != nil {
			log.Printf("remote online call not started (module %s, call %s, reason ice_fetch_failed, error %s)", a.deviceID, call.UUID, remoteErrorCode(err))
			return
		} // configured TURN failure must never downgrade to direct
		if err = credentials.Validate(time.Now()); err != nil {
			log.Printf("remote online call not started (module %s, call %s, reason ice_config_invalid)", a.deviceID, call.UUID)
			return
		}
		media.RelayOnly = credentials.RelayOnly
		for _, server := range credentials.Servers {
			media.ICEServers = append(media.ICEServers, webrtc.ICEServer{URLs: server.URLs, Username: server.Username, Credential: server.Credential})
		}
		if credentials.ExpiresAt != nil {
			iceExpiry = *credentials.ExpiresAt
			if !remoteTURNRenew {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, credentials.ExpiresAt.Add(-30*time.Second))
				defer cancel()
			}
		}
	}
	snapshot, err := a.runRemoteSignaling(ctx, client, media, nil, func(config *remotecall.SignalCall) {
		if remoteSignalResume {
			config.Reconnect, config.ResumeRefresh = manager.resume, manager.refresh
		}
		if remoteTURNRenew && !iceExpiry.IsZero() {
			config.ICEExpiresAt = iceExpiry
			config.RefreshICE = func(ctx context.Context) (turncred.Credentials, error) { return manager.ice(ctx, a.deviceID) }
		}
	})
	log.Printf("remote online call finished (module %s, call %s, state %s, reason %s, error %s, cleanup_failed %t)", a.deviceID, call.UUID, snapshot.State, snapshot.EndReason, remoteErrorCode(err), snapshot.CleanupError != "")
}
