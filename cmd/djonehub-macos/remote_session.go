package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotecall"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

type remoteOwnerContextKey struct{}
type remoteCallOwner struct {
	app     *app
	binding remotecall.Binding
}
type remoteSessionHandle struct {
	session *remotecall.Session
	binding remotecall.Binding
	native  *remoteNativeHost
	peer    *remotemedia.Peer
}
type remoteCallBackend struct {
	owner *remoteCallOwner
	// Supplied only by authenticated native host control, not the legacy
	// public audioHostRegister body. Nil intentionally never means ready.
	hostReady func(remotecall.Binding) bool
	mu        sync.Mutex
	claimed   bool
}

// runRemoteSignaling is the authenticated transport integration boundary. The
// Hub connection manager supplies its enrolled client. No public HTTP endpoint
// accepts identity or TURN credentials. Incoming dispatch is opt-in online-only;
// AI routing is checked before any media allocation or invitation.
func (a *app) runRemoteSignaling(ctx context.Context, client *callsignal.Client, mediaConfig remotemedia.PeerConfig, refreshAccess func(context.Context) (string, error)) (remotecall.Snapshot, error) {
	a.callMu.RLock()
	if a.activeCall == nil || a.activeCall.UUID != mediaConfig.CallUUID || a.activeCall.Route.Mode != callcontrol.Remote {
		a.callMu.RUnlock()
		return remotecall.Snapshot{}, remotecall.ErrRoute
	}
	call := *a.activeCall
	a.callMu.RUnlock()
	mediaConfig.Trickle = true // a blocked UDP TURN endpoint must not delay the SDP offer
	binding := remotecall.Binding{CallUUID: call.UUID, Generation: mediaConfig.Generation, InstallationID: call.Route.TargetInstallationID}
	config := remotecall.SignalCall{Config: remotecall.Config{Binding: binding, Route: call.Route, RingDeadline: call.StartedAt.Add(time.Duration(call.Route.RingTimeoutSeconds) * time.Second), Outgoing: call.Direction == "outgoing"}, ModuleID: a.deviceID, Display: "DJOneHub"}
	config.Display = a.deviceID
	if call.Direction == "outgoing" || call.Route.CallerDisplay == "number" {
		if call.Number != "" && len(call.Number) <= 128 {
			config.Display = call.Number
		}
	}
	if config.Display == "" {
		config.Display = "DJOneHub"
	}
	config.RefreshAccess = refreshAccess
	return remotecall.RunSignaledCall(ctx, client, config, func(ctx context.Context) (*remotecall.Session, remotecall.SignalMedia, error) {
		var peer *remotemedia.Peer
		session, err := a.startRemoteSession(ctx, binding, nil, func(ctx context.Context) (remotecall.Media, error) {
			var err error
			peer, err = remotemedia.NewPeer(ctx, mediaConfig)
			return peer, err
		})
		return session, peer, err
	})
}

func (a *app) cancelRemoteInvitation(uuid string) {
	a.remoteSessionMu.Lock()
	var session *remotecall.Session
	if a.remoteSession != nil && a.remoteSession.binding.CallUUID == uuid {
		session = a.remoteSession.session
	}
	a.remoteSessionMu.Unlock()
	if session != nil {
		session.AnsweredElsewhere()
	}
}

// startRemoteSession is an internal integration boundary. The paired
// signaling connection supplies installationID and a new media generation.
// There is intentionally no HTTP handler that accepts these as proof of identity.
func (a *app) startRemoteSession(parent context.Context, binding remotecall.Binding, hostReady func(remotecall.Binding) bool, factory func(context.Context) (remotecall.Media, error)) (*remotecall.Session, error) {
	a.remoteSessionMu.Lock()
	defer a.remoteSessionMu.Unlock()
	if a.remoteSession != nil {
		select {
		case <-a.remoteSession.session.Done():
			if a.remoteSession.binding.CallUUID == binding.CallUUID && binding.Generation <= a.remoteSession.binding.Generation {
				return nil, remotecall.ErrBinding
			}
		default:
			return nil, remotecall.ErrState
		}
	}
	a.callMu.Lock()
	if a.activeCall == nil || a.activeCall.UUID != binding.CallUUID || !remoteClaimable(a.activeCall) {
		a.callMu.Unlock()
		return nil, remotecall.ErrState
	}
	a.ensureCallIdentityLocked()
	call, callCtx := *a.activeCall, a.callContext
	a.callMu.Unlock()
	ctx, cancel := context.WithCancel(callCtx)
	stop := context.AfterFunc(parent, cancel)
	if err := parent.Err(); err != nil {
		stop()
		cancel()
		return nil, err
	}
	var native *remoteNativeHost
	var peer *remotemedia.Peer
	backend := &remoteCallBackend{owner: &remoteCallOwner{app: a, binding: binding}, hostReady: hostReady}
	if hostReady == nil {
		backend.hostReady = func(binding remotecall.Binding) bool { return native != nil && native.ready(binding) }
	}
	wrappedFactory := func(ctx context.Context) (remotecall.Media, error) {
		media, err := factory(ctx)
		if err == nil {
			peer, _ = media.(*remotemedia.Peer)
			if provider, ok := media.(interface {
				Host() *remotemedia.HostBridge
			}); ok {
				bridge := provider.Host()
				if bridge == nil || bridge.Binding().CallUUID != binding.CallUUID || bridge.Binding().Generation != binding.Generation {
					media.Close()
					return nil, remotecall.ErrBinding
				}
				native = &remoteNativeHost{bridge: bridge, binding: binding}
			}
		}
		return media, err
	}
	deadline := call.StartedAt.Add(time.Duration(call.Route.RingTimeoutSeconds) * time.Second)
	if factory == nil {
		stop()
		cancel()
		return nil, remotecall.ErrState
	}
	session, err := remotecall.New(ctx, remotecall.Config{Binding: binding, Route: call.Route, RingDeadline: deadline, Outgoing: call.Direction == "outgoing"}, backend, wrappedFactory)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	a.remoteSession = &remoteSessionHandle{session: session, binding: binding, native: native, peer: peer}
	go func() { <-session.Done(); stop(); cancel() }()
	return session, nil
}

func (b *remoteCallBackend) Claim(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a, binding := b.owner.app, b.owner.binding
	arbiter := a.audioArbiter()
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	call := a.activeCall
	if call == nil || call.UUID != binding.CallUUID || !remoteClaimable(call) || a.remoteCallOwner != nil {
		return remotecall.ErrState
	}
	if call.Route.Mode != callcontrol.Remote || call.Route.TargetInstallationID != binding.InstallationID {
		return remotecall.ErrRoute
	}
	lease, err := arbiter.Acquire(callcontrol.AudioOwner{DeviceID: a.deviceID, CallUUID: binding.CallUUID})
	if err != nil {
		return err
	}
	a.callAudioLease, a.remoteCallOwner = lease, b.owner
	call.ControlOwner = "remote"
	call.Revision++
	b.mu.Lock()
	b.claimed = true
	b.mu.Unlock()
	return nil
}
func (b *remoteCallBackend) command(ctx context.Context, action string) (callcontrol.Result, error) {
	ctx = context.WithValue(ctx, remoteOwnerContextKey{}, b.owner)
	return b.owner.app.executeCallCommand(ctx, callcontrol.Command{RequestID: callcontrol.NewCallUUID(), CallUUID: b.owner.binding.CallUUID, Type: action, Actor: "remote"})
}
func remoteClaimable(call *callRecord) bool {
	return call != nil && ((call.State == "incoming" && call.ControlState == "" && call.ControlOwner == "") || (call.Direction == "outgoing" && call.State == "dialing" && call.ControlState == "dial_preparing" && call.ControlOwner == "remote"))
}
func (b *remoteCallBackend) Answer(ctx context.Context) (bool, error) {
	a := b.owner.app
	a.callMu.RLock()
	outgoing := a.activeCall != nil && a.activeCall.UUID == b.owner.binding.CallUUID && a.activeCall.Direction == "outgoing"
	a.callMu.RUnlock()
	if outgoing {
		return b.dial(ctx)
	}
	result, err := b.command(ctx, "answer")
	return result.MayHaveAnswered, err
}
func (b *remoteCallBackend) Reject(ctx context.Context) error {
	a := b.owner.app
	a.callMu.RLock()
	outgoing := a.activeCall != nil && a.activeCall.UUID == b.owner.binding.CallUUID && a.activeCall.Direction == "outgoing"
	a.callMu.RUnlock()
	if outgoing {
		return nil
	} // no ATD was submitted; cancellation is local cleanup
	_, err := b.command(ctx, "reject")
	return err
}
func (b *remoteCallBackend) Observe() (remotecall.Observation, error) {
	a := b.owner.app
	a.callMu.RLock()
	call := a.activeCall
	if call == nil || call.UUID != b.owner.binding.CallUUID {
		a.callMu.RUnlock()
		return remotecall.Observation{Ended: true}, nil
	}
	if a.remoteCallOwner != b.owner {
		a.callMu.RUnlock()
		return remotecall.Observation{}, remotecall.ErrBinding
	}
	active := call.State == "active"
	a.callMu.RUnlock()
	a.moduleVoiceMu.Lock()
	moduleReady := a.moduleVoiceReady
	a.moduleVoiceMu.Unlock()
	return remotecall.Observation{CellularActive: active, ModuleReady: moduleReady && b.hostReady != nil && b.hostReady(b.owner.binding)}, nil
}
func (b *remoteCallBackend) Finish(ctx context.Context, mayHaveAnswered bool) error {
	b.mu.Lock()
	claimed := b.claimed
	b.mu.Unlock()
	if !claimed {
		return nil
	}
	a := b.owner.app
	if mayHaveAnswered {
		// Bind a newly allocated outgoing modem index before using bound ATH.
		if err := b.reconcileOutgoing(ctx); err != nil {
			return err
		}
		// Even an unknown ATA outcome needs fresh CLCC plus bound hangup. Never
		// retry ATA or reinterpret a replacement as the original cellular call.
		if _, err := b.command(ctx, "hangup"); err != nil {
			return err
		}
	}
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if a.remoteCallOwner != b.owner {
		return nil
	}
	call := a.activeCall
	if call == nil || call.UUID != b.owner.binding.CallUUID {
		return errors.New("remote ownership outlived its call")
	}
	if !mayHaveAnswered && call.Direction == "outgoing" && call.ControlState == "dial_preparing" {
		call.ControlState = "ending_pending"
		a.callMu.Unlock()
		a.applyCallPoll(nil, time.Now())
		a.callMu.Lock()
		return nil
	}
	if !mayHaveAnswered && call.State == "incoming" && call.ControlState == "" {
		a.audioArbiter().Release(a.callAudioLease)
		a.callAudioLease = callcontrol.AudioLease{}
		a.remoteCallOwner = nil
		call.ControlOwner = ""
		call.Revision++
	}
	// Active/answer-pending/ending-pending calls retain ownership until CLCC
	// teardown releases the exact call lease through the normal lifecycle.
	return nil
}
