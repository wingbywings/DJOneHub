package relay

import (
	"context"
	"github.com/iniwex5/vohive/internal/callsignal"
	"time"
)

// Called with broker.mu held. Pending routes are memory-only and bound to the
// original live Hub connection; process restart cannot resurrect a call.
func (b *signalBroker) wake(hub *signalConnection, m callsignal.Message, invite callsignal.Invite) string {
	if b.push == nil {
		return "target_unavailable"
	}
	select {
	case b.pushSlots <- struct{}{}:
	default:
		return "push_capacity"
	}
	ctx, stop := context.WithDeadline(hub.ctx, invite.Deadline)
	route := &signalRoute{invite: m, hub: hub, stopWake: stop}
	b.routes[hub.principal.ID] = route
	hub.seen[m.CallUUID] = true
	ack := m
	ack.Type = "waking"
	ack.Payload = callsignal.Body(callsignal.Result{Accepted: true})
	hub.send(ack)
	b.workers.Add(1)
	go func() {
		defer b.workers.Done()
		defer func() { <-b.pushSlots }()
		defer stop()
		// Authenticate again immediately before resolving the private push address.
		hub.authMu.Lock()
		token := hub.token
		hub.authMu.Unlock()
		endpoint, err := b.store.PushEndpoint(ctx, token, m.InstallationID, m.ModuleID)
		if err == nil && endpoint.Environment != b.push.Environment() {
			err = context.Canceled
		}
		if err == nil && ctx.Err() == nil {
			result, sendErr := b.push.Send(ctx, endpoint.Token, m)
			b.pushActivity.Record(sendErr == nil)
			err = sendErr
			if result.InvalidToken {
				_ = b.store.InvalidatePushEndpoint(context.Background(), endpoint, result.InvalidatedAt)
			}
		}
		if err == nil {
			<-ctx.Done()
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.routes[hub.principal.ID] != route || route.phone != nil {
			return
		}
		delete(b.routes, hub.principal.ID)
		failure := m
		failure.Type = "error"
		code := "push_unavailable"
		if !invite.Deadline.After(time.Now()) {
			code = "invite_expired"
		}
		failure.Payload = callsignal.Body(callsignal.Failure{Code: code})
		hub.send(failure)
	}()
	return ""
}
func (b *signalBroker) attachPending(phone *signalConnection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	phone.ready = true
	if phone.principal.Kind != "installation" {
		return
	}
	route := b.routes[phone.principal.HubID]
	if route == nil || route.phone != nil || route.invite.InstallationID != phone.principal.ID {
		return
	}
	var invite callsignal.Invite
	callsignal.Decode(route.invite.Payload, &invite)
	if !invite.Deadline.After(time.Now()) || route.hub.ctx.Err() != nil || route.hub.principal.Tenant != phone.principal.Tenant {
		return
	}
	if _, err := phone.authenticate(b.store, route.invite.ModuleID); err != nil {
		return
	}
	if _, err := route.hub.authenticate(b.store, route.invite.ModuleID); err != nil {
		return
	}
	route.phone = phone
	if route.stopWake != nil {
		route.stopWake()
	}
	if !phone.send(route.invite) {
		route.hub.cancel()
		return
	}
	ack := route.invite
	ack.Type = "delivered"
	ack.Payload = callsignal.Body(callsignal.Result{Accepted: true})
	route.hub.send(ack)
}
