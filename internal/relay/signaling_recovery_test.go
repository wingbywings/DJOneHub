//go:build cgo

package relay

import (
	"context"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
)

func recoveryDial(t *testing.T, f *signalFixture, token identity.Tokens, resume *callsignal.Message) *callsignal.Client {
	t.Helper()
	config := f.config(token)
	config.Resumption, config.Resume = true, resume
	c, err := callsignal.Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func recoveryRoute(t *testing.T, f *signalFixture) (*callsignal.Client, *callsignal.Client, callsignal.Message) {
	t.Helper()
	phone, hub := recoveryDial(t, f, f.phone, nil), recoveryDial(t, f, f.hub, nil)
	invite := invitation(hub, f.phone)
	sendSignal(t, hub, invite)
	receive(t, phone, "invite")
	receive(t, hub, "delivered")
	state := invite
	state.Type, state.Payload = "state", callsignal.Body(callsignal.State{Revision: 5, State: "connected"})
	sendSignal(t, hub, state)
	receive(t, phone, "state")
	return hub, phone, invite
}

func TestSignalResumePreservesBindingWithoutReplayingCommands(t *testing.T) {
	for _, side := range []string{"phone", "hub"} {
		t.Run(side, func(t *testing.T) {
			f := signalSetup(t)
			hub, phone, invite := recoveryRoute(t, f)
			oldEpoch := hub.Epoch()
			if side == "phone" {
				phone.Close()
				receive(t, hub, "peer_reconnecting")
				phone = recoveryDial(t, f, f.phone, &invite)
			} else {
				hub.Close()
				receive(t, phone, "peer_reconnecting")
				hub = recoveryDial(t, f, f.hub, &invite)
			}
			if hub.Epoch() != oldEpoch {
				t.Fatal("active call epoch changed")
			}
			if !receive(t, hub, "peer_resumed").SameCall(invite) || !receive(t, phone, "peer_resumed").SameCall(invite) {
				t.Fatal("binding changed")
			}
			command := invite
			command.Type, command.Payload = "command", callsignal.Body(callsignal.Command{Action: "hangup", ExpectedRevision: 5})
			sendSignal(t, phone, command)
			if receive(t, hub, "command").RequestID != command.RequestID {
				t.Fatal("request identity changed")
			}
			select {
			case extra := <-hub.Messages():
				t.Fatal("unexpected replay", extra.Type)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestSignalResumeCannotTakeOverDifferentCallOrIdentity(t *testing.T) {
	f := signalSetup(t)
	hub, phone, invite := recoveryRoute(t, f)
	phone.Close()
	receive(t, hub, "peer_reconnecting")
	for _, kind := range []string{"generation", "epoch", "module", "installation", "identity", "capability"} {
		config := f.config(f.phone)
		binding := invite
		config.Resumption, config.Resume = true, &binding
		switch kind {
		case "generation":
			binding.Generation++
		case "epoch":
			binding.HubEpoch = "different"
		case "module":
			binding.ModuleID = "other"
		case "installation":
			binding.InstallationID = "different"
		case "identity":
			config = f.config(f.pair(t, f.hub, "module"))
			config.Resumption, config.Resume = true, &binding
		case "capability":
			config.Resumption = false
		}
		if c, err := callsignal.Dial(context.Background(), config); err == nil {
			c.Close()
			t.Fatal("invalid resume accepted", kind)
		}
	}
	phone = recoveryDial(t, f, f.phone, &invite)
	receive(t, phone, "peer_resumed")
	receive(t, hub, "peer_resumed")
}

func TestSignalResumeCanReplaceHalfOpenSocketAndRejectOldConnection(t *testing.T) {
	f := signalSetup(t)
	hub, old, invite := recoveryRoute(t, f)
	phone := recoveryDial(t, f, f.phone, &invite)
	receive(t, hub, "peer_reconnecting")
	receive(t, hub, "peer_resumed")
	receive(t, phone, "peer_resumed")
	select {
	case <-old.Done():
	case <-time.After(time.Second):
		t.Fatal("superseded socket alive")
	}
	state := invite
	state.Type, state.Payload = "state", callsignal.Body(callsignal.State{Revision: 6, State: "ended"})
	sendSignal(t, hub, state)
	receive(t, phone, "state")
	config := f.config(f.phone)
	config.Resumption, config.Resume = true, &invite
	if c, err := callsignal.Dial(context.Background(), config); err == nil {
		c.Close()
		t.Fatal("ended call resumed")
	}
}

func TestSignalResumeNeverRetainsRingingOrLegacyCall(t *testing.T) {
	f := signalSetup(t)
	phone, hub := recoveryDial(t, f, f.phone, nil), recoveryDial(t, f, f.hub, nil)
	invite := invitation(hub, f.phone)
	sendSignal(t, hub, invite)
	receive(t, hub, "delivered")
	receive(t, phone, "invite")
	phone.Close()
	receive(t, hub, "peer_closed")
	config := f.config(f.phone)
	config.Resumption, config.Resume = true, &invite
	if c, err := callsignal.Dial(context.Background(), config); err == nil {
		c.Close()
		t.Fatal("ringing call resumed")
	}
}

func TestSignalResumeBothEndpointsShareFixedDeadline(t *testing.T) {
	f := signalSetup(t)
	hub, phone, invite := recoveryRoute(t, f)
	phone.Close()
	receive(t, hub, "peer_reconnecting")
	b := f.server.signals
	b.mu.Lock()
	route := b.routes[f.hub.Principal.HubID]
	deadline := route.suspendedUntil
	b.mu.Unlock()
	// A state sent during the outage is rejected; it cannot wait in a queue
	// and later masquerade as fresh state on the resumed socket.
	state := invite
	state.Type, state.Payload = "state", callsignal.Body(callsignal.State{Revision: 6, State: "connected"})
	sendSignal(t, hub, state)
	if errorCode(t, receive(t, hub, "error")) != "peer_reconnecting" {
		t.Fatal("outage accepted state")
	}
	hub.Close()
	hub = recoveryDial(t, f, f.hub, &invite)
	b.mu.Lock()
	unchanged := route.suspendedUntil == deadline
	b.mu.Unlock()
	if !unchanged {
		t.Fatal("second disconnect/resume extended deadline")
	}
	phone = recoveryDial(t, f, f.phone, &invite)
	receive(t, hub, "peer_resumed")
	receive(t, phone, "peer_resumed")
	// The timer from the old outage must not kill a resumed call.
	b.expireSuspension(route, deadline)
	state.Payload = callsignal.Body(callsignal.State{Revision: 7, State: "connected"})
	sendSignal(t, hub, state)
	receive(t, phone, "state")
}

func TestSignalResumeExpiredAndRevokedCallsStayEnded(t *testing.T) {
	for _, reason := range []string{"timeout", "revoked"} {
		t.Run(reason, func(t *testing.T) {
			f := signalSetup(t)
			hub, phone, invite := recoveryRoute(t, f)
			phone.Close()
			receive(t, hub, "peer_reconnecting")
			if reason == "timeout" {
				b := f.server.signals
				b.mu.Lock()
				route := b.routes[f.hub.Principal.HubID]
				deadline := route.suspendedUntil
				b.mu.Unlock()
				b.expireSuspension(route, deadline)
				receive(t, hub, "peer_closed")
			} else if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
				t.Fatal(err)
			}
			config := f.config(f.phone)
			config.Resumption, config.Resume = true, &invite
			if c, err := callsignal.Dial(context.Background(), config); err == nil {
				c.Close()
				t.Fatal("invalid resume", reason)
			}
		})
	}
}
