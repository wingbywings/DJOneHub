package relay

import (
	"context"
	"encoding/base64"
	"slices"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
)

const signalRecoveryBudget = 10 * time.Second

func samePrincipal(a, b identity.Principal) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.HubID == b.HubID &&
		a.Tenant == b.Tenant && a.Version == b.Version && slices.Equal(a.Modules, b.Modules)
}

// Called with broker.mu held. Check detached endpoints with a fresh context:
// their socket context is already cancelled, but revoked/expired credentials
// must still terminate a route rather than buy a recovery grace period.
func (b *signalBroker) canSuspend(route *signalRoute) bool {
	if b.closed || !route.connected || route.phone == nil || !route.hub.recovery || !route.phone.recovery {
		return false
	}
	for _, c := range []*signalConnection{route.hub, route.phone} {
		c.authMu.Lock()
		token := c.token
		c.authMu.Unlock()
		p, err := b.store.AuthorizeModule(context.Background(), token, c.principal.HubID, route.invite.ModuleID)
		if err != nil || !samePrincipal(p, c.principal) {
			return false
		}
	}
	return true
}

// The fixed deadline is not extended by a second disconnect, a failed upgrade,
// or a retry. No audio, SDP or hardware commands are stored for replay.
func (b *signalBroker) suspend(route *signalRoute) {
	if !route.suspendedUntil.IsZero() {
		return
	}
	deadline := time.Now().Add(signalRecoveryBudget)
	route.suspendedUntil = deadline
	m := route.invite
	m.Type, m.Payload = "peer_reconnecting", callsignal.Body(callsignal.Failure{Code: "signal_reconnecting"})
	route.hub.send(m)
	route.phone.send(m)
	time.AfterFunc(signalRecoveryBudget, func() { b.expireSuspension(route, deadline) })
}

func (b *signalBroker) expireSuspension(route *signalRoute, deadline time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.routes[route.hub.principal.HubID] != route || route.suspendedUntil != deadline {
		return
	}
	delete(b.routes, route.hub.principal.HubID)
	m := route.invite
	m.Type, m.Payload = "peer_closed", callsignal.Body(callsignal.Failure{Code: "signal_recovery_timeout"})
	route.hub.send(m)
	route.phone.send(m)
}

// Resumption is possession of a fresh authorized access credential PLUS the
// exact original call binding, with an unchanged tenant/version/module scope.
// It cannot create a route or recover a ringing/ended/restarted Relay call.
func (b *signalBroker) reserveConnection(c *signalConnection, encoded string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	if encoded == "" {
		if len(b.connections) >= 128 || b.connections[c.principal.ID] != nil {
			return false
		}
		// An ordinary connect must not strand or implicitly steal a suspended call.
		if route := b.routes[c.principal.HubID]; route != nil && !route.suspendedUntil.IsZero() {
			return false
		}
		b.connections[c.principal.ID] = c
		return true
	}
	if !c.recovery {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var request callsignal.Message
	var empty struct{}
	if err != nil || len(data) > 1536 || callsignal.Decode(data, &request) != nil || request.Type != "resume" || callsignal.Decode(request.Payload, &empty) != nil {
		return false
	}
	// Validate the full binding using a direction-checked, empty ICE payload.
	binding := request
	binding.Type, binding.Payload = "ice", callsignal.Body(callsignal.ICE{})
	if binding.Validate(c.principal.Kind) != nil {
		return false
	}
	route := b.routes[c.principal.HubID]
	if route == nil || !route.invite.SameCall(request) || !b.canSuspend(route) ||
		(!route.suspendedUntil.IsZero() && !time.Now().Before(route.suspendedUntil)) {
		return false
	}
	old := route.phone
	if c.principal.Kind == "hub" {
		old = route.hub
	}
	if !samePrincipal(c.principal, old.principal) {
		return false
	}
	if current := b.connections[c.principal.ID]; current != nil && current != old {
		return false
	}
	if b.connections[c.principal.ID] == nil && len(b.connections) >= 128 {
		return false
	}
	b.suspend(route)
	c.epoch, c.seen, c.resumed = old.epoch, old.seen, true
	if c.principal.Kind == "hub" {
		route.hub = c
	} else {
		route.phone = c
	}
	b.connections[c.principal.ID] = c
	old.cancel()
	return true
}

func (b *signalBroker) finishResume(c *signalConnection) {
	if !c.resumed {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	route := b.routes[c.principal.HubID]
	if route == nil || (route.hub != c && route.phone != c) || route.suspendedUntil.IsZero() ||
		!time.Now().Before(route.suspendedUntil) || !route.hub.ready || !route.phone.ready ||
		route.hub.ctx.Err() != nil || route.phone.ctx.Err() != nil {
		return
	}
	route.suspendedUntil = time.Time{}
	m := route.invite
	m.Type, m.Payload = "peer_resumed", callsignal.Body(callsignal.Failure{Code: "signal_resumed"})
	route.hub.send(m)
	route.phone.send(m)
}
