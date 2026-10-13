package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/relayprofile"
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
	resume   func(context.Context, callsignal.Message) (*callsignal.Client, error)
	refresh  func(context.Context) (string, error)
	ice      func(context.Context, string) (turncred.Credentials, error)
	activate func()
}

// An explicit integration entry point until background-call device validation is complete.
// The authenticated Web manager may select profiles via its serialized job
// boundary; public device/status routes never load Keychain credentials.
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
	service, err := startRemoteHubWithStore(ctx, profile, nativeFile, store, incoming)
	if err == nil {
		service.activate()
	}
	return service, err
}
func startRemoteHubWithStore(ctx context.Context, profile, nativeFile string, store *relayprofile.Store, incoming func(context.Context, remotehub.Connection, callsignal.Message), moduleNames ...func([]string) []identity.ModuleChoice) (*remoteHubService, error) {
	if profile == "" || nativeFile == "" || !opus.Available() {
		return nil, relayprofile.ErrInvalid
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
	ready := make(chan struct{})
	var once sync.Once
	var accessMu sync.RWMutex
	var access string
	setAccess := func(value string) { accessMu.Lock(); access = value; accessMu.Unlock() }
	refreshAccess := func(ctx context.Context) (string, error) {
		token, err := store.RefreshAccess(ctx, profile)
		if err == nil {
			setAccess(token)
		}
		return token, err
	}
	startNames := func(config callsignal.ClientConfig, client *callsignal.Client) {
		setAccess(config.AccessToken)
		if len(moduleNames) == 0 || moduleNames[0] == nil {
			return
		}
		// Use the service lifetime even when the socket resumes an active call.
		go publishRemoteModuleNames(ctx, client.Done(), status.RelayURL,
			relayclient.Options{AllowInsecureLoopback: config.AllowInsecureLoopback, TLSConfig: config.TLSConfig},
			func() string { accessMu.RLock(); defer accessMu.RUnlock(); return access },
			func() []identity.ModuleChoice { return moduleNames[0](status.Principal.Modules) })
	}
	manager := remotehub.Start(ctx, remotehub.Config{
		RecoverCalls: remoteSignalResume,
		Connect: func(ctx context.Context) (remotehub.Connection, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
			}
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
			startNames(config, client)
			return client, nil
		},
		Refresh:  refreshAccess,
		Incoming: incoming,
	})
	return &remoteHubService{Manager: manager, activate: func() { once.Do(func() { close(ready) }) },
		resume: func(ctx context.Context, binding callsignal.Message) (*callsignal.Client, error) {
			config, err := store.ConnectionConfig(ctx, profile)
			if err != nil {
				return nil, err
			}
			config.Resumption, config.Resume = true, &binding
			client, err := callsignal.Dial(ctx, config)
			if err == nil {
				startNames(config, client)
			}
			return client, err
		},
		refresh: refreshAccess, ice: func(ctx context.Context, module string) (turncred.Credentials, error) {
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
