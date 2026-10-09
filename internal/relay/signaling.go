package relay

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"golang.org/x/time/rate"
)

// All routes are bound to these exact connection objects. A reconnect starts a
// new epoch and never resumes or replays an earlier hardware command.
type signalConnection struct {
	principal         identity.Principal
	token, epoch      string
	authMu            sync.Mutex // guards token and authorization watcher replacement
	authorization     context.Context
	stopAuthorization context.CancelFunc
	ctx               context.Context
	cancel            context.CancelFunc
	ws                *websocket.Conn
	out               chan callsignal.Message
	seen              map[string]bool // bounded call tombstones for this Hub connection
}
type signalRoute struct {
	invite     callsignal.Message
	hub, phone *signalConnection
	iceCount   int
	outgoing   bool
	invited    bool
}
type signalBroker struct {
	store       *identity.Store
	mu          sync.Mutex
	closed      bool
	connections map[string]*signalConnection
	routes      map[string]*signalRoute // first version: one live call per Hub
}

func newSignalBroker(store *identity.Store) *signalBroker {
	return &signalBroker{store: store, connections: make(map[string]*signalConnection), routes: make(map[string]*signalRoute)}
}
func (b *signalBroker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, c := range b.connections {
		c.cancel()
	}
}
func (c *signalConnection) send(m callsignal.Message) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case c.out <- m:
		return true
	default:
		c.cancel()
		return false
	}
}
func (b *signalBroker) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || r.URL.RawQuery != "" || len(websocket.Subprotocols(r)) != 1 || websocket.Subprotocols(r)[0] != callsignal.Subprotocol {
		respond(w, http.StatusBadRequest, callsignal.Failure{Code: "invalid_signal_handshake"})
		return
	}
	token := identity.Bearer(r.Header.Get("Authorization"))
	p, err := b.store.Authenticate(r.Context(), token)
	if err != nil || len(p.Modules) == 0 {
		respond(w, http.StatusUnauthorized, callsignal.Failure{Code: "invalid_credential"})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	authorization, stopAuthorization, err := b.store.AuthorizeContext(ctx, token, p.HubID, p.Modules[0])
	if err != nil {
		respond(w, http.StatusUnauthorized, callsignal.Failure{Code: "invalid_credential"})
		return
	}
	c := &signalConnection{principal: p, token: token, epoch: uuid.NewString(), ctx: ctx, cancel: cancel, authorization: authorization, stopAuthorization: stopAuthorization, out: make(chan callsignal.Message, 8), seen: make(map[string]bool)}
	defer func() { c.authMu.Lock(); c.stopAuthorization(); c.authMu.Unlock() }()
	b.mu.Lock()
	if b.closed || len(b.connections) >= 128 || b.connections[p.ID] != nil {
		b.mu.Unlock()
		respond(w, http.StatusConflict, callsignal.Failure{Code: "connection_unavailable"})
		return
	}
	b.connections[p.ID] = c
	b.mu.Unlock()
	defer b.detach(c)
	u := websocket.Upgrader{Subprotocols: []string{callsignal.Subprotocol}, HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	c.ws, err = u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.ws.Close()
	stop := context.AfterFunc(ctx, func() { c.ws.Close() })
	defer stop()
	c.ws.SetReadLimit(callsignal.MaxMessage)
	limit := rate.NewLimiter(20, 40)
	c.ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	c.ws.SetPongHandler(func(string) error {
		if !limit.Allow() {
			return callsignal.ErrProtocol
		}
		return c.ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	})
	c.ws.SetPingHandler(func(data string) error {
		if !limit.Allow() {
			return callsignal.ErrProtocol
		}
		return c.ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	c.send(callsignal.Message{Version: 1, Type: "welcome", HubEpoch: c.epoch, Payload: callsignal.Body(callsignal.Welcome{ID: p.ID, Kind: p.Kind, HubID: p.HubID, AccessExpiresAt: p.AccessExpiresAt})})
	writerDone := make(chan struct{})
	authDone := make(chan struct{})
	go func() { defer close(authDone); c.watchAuthorization() }()
	go func() { defer close(writerDone); b.write(c) }()
	defer func() { cancel(); c.ws.Close(); <-writerDone; <-authDone }()
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var m callsignal.Message
		if kind != websocket.TextMessage || !limit.Allow() || callsignal.Decode(data, &m) != nil || m.Validate(p.Kind) != nil {
			return
		}
		if m.Type == "reauth" {
			var proof callsignal.Reauthorize
			callsignal.Decode(m.Payload, &proof)
			expires, err := b.reauthorize(c, proof.AccessToken)
			if err != nil {
				return
			} // never reflect a credential body into an error or route
			if !c.send(callsignal.Message{Version: 1, Type: "reauthenticated", RequestID: m.RequestID, Payload: callsignal.Body(callsignal.Authorization{AccessExpiresAt: expires})}) {
				return
			}
			continue
		}
		if code := b.route(c, m); code != "" {
			m.Type, m.Payload = "error", callsignal.Body(callsignal.Failure{Code: code})
			if !c.send(m) {
				return
			}
		}
	}
}
func (b *signalBroker) write(c *signalConnection) {
	defer c.cancel()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
			if c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		case m := <-c.out:
			// Recheck even queued messages so revocation does not permit a stale
			// socket to receive another caller's metadata or SDP indefinitely.
			if _, err := c.authenticate(b.store, ""); err != nil {
				return
			}
			c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if c.ws.WriteJSON(m) != nil {
				return
			}
		}
	}
}
func (b *signalBroker) route(c *signalConnection, m callsignal.Message) string {
	if _, err := c.authenticate(b.store, m.ModuleID); err != nil {
		c.cancel()
		return "unauthorized"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || c.ctx.Err() != nil || b.connections[c.principal.ID] != c {
		return "connection_ended"
	}
	route := b.routes[c.principal.HubID]
	if m.Type == "dial" {
		if m.HubEpoch != c.epoch || m.InstallationID != c.principal.ID {
			return "unauthorized"
		}
		if route != nil || c.seen[m.CallUUID] {
			return "call_unavailable"
		}
		if len(c.seen) >= 1024 {
			c.cancel()
			return "connection_capacity"
		}
		hub := b.connections[c.principal.HubID]
		if hub == nil || hub.principal.Kind != "hub" || hub.principal.Tenant != c.principal.Tenant {
			return "target_unavailable"
		}
		if _, err := hub.authenticate(b.store, m.ModuleID); err != nil {
			return "target_unavailable"
		}
		if hub.seen[m.CallUUID] {
			return "call_unavailable"
		}
		if len(hub.seen) >= 1024 {
			return "connection_capacity"
		}
		hub.seen[m.CallUUID] = true
		c.seen[m.CallUUID] = true
		m.HubEpoch = hub.epoch
		b.routes[c.principal.HubID] = &signalRoute{invite: m, hub: hub, phone: c, outgoing: true}
		if !hub.send(m) {
			delete(b.routes, c.principal.HubID)
			return "target_unavailable"
		}
		ack := m
		ack.Type = "dial_accepted"
		ack.Payload = callsignal.Body(callsignal.Result{Accepted: true})
		c.send(ack)
		return ""
	}
	if m.Type == "cancel_dial" {
		if route == nil || !route.outgoing || route.phone != c || m.InstallationID != c.principal.ID || m.CallUUID != route.invite.CallUUID || m.ModuleID != route.invite.ModuleID || m.Generation != route.invite.Generation || (m.HubEpoch != c.epoch && m.HubEpoch != route.invite.HubEpoch) {
			return "call_unavailable"
		}
		m.HubEpoch = route.invite.HubEpoch
		m.Type = "command"
		m.Payload = callsignal.Body(callsignal.Command{Action: "reject", ExpectedRevision: 1})
	}
	if m.Type == "invite" {
		if m.HubEpoch != c.epoch {
			return "stale_epoch"
		}
		var invite callsignal.Invite
		callsignal.Decode(m.Payload, &invite)
		if !invite.Deadline.After(time.Now()) || invite.Deadline.After(time.Now().Add(90*time.Second)) {
			return "invalid_deadline"
		}
		if route != nil && route.outgoing && !route.invited && route.hub == c && route.invite.SameCall(m) && invite.Direction == "outgoing" {
			route.invited = true
			route.invite = m
			if !route.phone.send(m) {
				return "target_unavailable"
			}
			ack := m
			ack.Type = "delivered"
			ack.Payload = callsignal.Body(callsignal.Result{Accepted: true})
			c.send(ack)
			return ""
		}
		if invite.Direction == "outgoing" || route != nil || c.seen[m.CallUUID] {
			return "call_unavailable"
		}
		if len(c.seen) >= 1024 {
			c.cancel()
			return "connection_capacity"
		}
		phone := b.connections[m.InstallationID]
		if phone == nil || phone.principal.Kind != "installation" || phone.principal.HubID != c.principal.ID || phone.principal.Tenant != c.principal.Tenant {
			return "target_unavailable"
		}
		if _, err := phone.authenticate(b.store, m.ModuleID); err != nil {
			phone.cancel()
			return "target_unavailable"
		}
		c.seen[m.CallUUID] = true
		b.routes[c.principal.ID] = &signalRoute{invite: m, hub: c, phone: phone}
		if !phone.send(m) {
			delete(b.routes, c.principal.ID)
			return "target_unavailable"
		}
		ack := m
		ack.Type = "delivered"
		ack.Payload = callsignal.Body(callsignal.Result{Accepted: true})
		c.send(ack) // enqueued, not CallKit reported / not cellular answered
		return ""
	}
	if route == nil || !route.invite.SameCall(m) {
		return "call_unavailable"
	}
	var target *signalConnection
	if c == route.hub {
		target = route.phone
	} else if c == route.phone {
		target = route.hub
	} else {
		return "unauthorized"
	}
	if _, err := target.authenticate(b.store, m.ModuleID); err != nil {
		target.cancel()
		return "target_unavailable"
	}
	if m.Type == "ice" {
		route.iceCount++
		if route.iceCount > 256 {
			return "candidate_limit"
		}
	}
	if !target.send(m) {
		return "target_unavailable"
	}
	if m.Type == "state" {
		var state callsignal.State
		callsignal.Decode(m.Payload, &state)
		if state.State == "ended" {
			delete(b.routes, c.principal.HubID)
		}
	}
	return ""
}

func (c *signalConnection) authenticate(store *identity.Store, module string) (identity.Principal, error) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if module == "" {
		return store.Authenticate(c.ctx, c.token)
	}
	return store.AuthorizeModule(c.ctx, c.token, c.principal.HubID, module)
}
func (c *signalConnection) watchAuthorization() {
	for {
		c.authMu.Lock()
		current := c.authorization
		c.authMu.Unlock()
		select {
		case <-c.ctx.Done():
			return
		case <-current.Done():
			c.authMu.Lock()
			if c.authorization == current {
				c.cancel()
				c.authMu.Unlock()
				return
			}
			c.authMu.Unlock() // old token watcher was replaced by successful reauth
		}
	}
}
func (b *signalBroker) reauthorize(c *signalConnection, token string) (time.Time, error) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if _, err := b.store.Authenticate(c.ctx, c.token); err != nil {
		return time.Time{}, err
	}
	p, err := b.store.Authenticate(c.ctx, token)
	if err != nil {
		return time.Time{}, err
	}
	old := c.principal
	if p.ID != old.ID || p.Kind != old.Kind || p.HubID != old.HubID || p.Tenant != old.Tenant || p.Version != old.Version || !slices.Equal(p.Modules, old.Modules) {
		return time.Time{}, identity.ErrUnauthorized
	}
	watch, stop, err := b.store.AuthorizeContext(c.ctx, token, p.HubID, p.Modules[0])
	if err != nil {
		return time.Time{}, err
	}
	if c.ctx.Err() != nil {
		stop()
		return time.Time{}, c.ctx.Err()
	}
	previousStop := c.stopAuthorization
	c.token, c.authorization, c.stopAuthorization = token, watch, stop
	previousStop()
	return p.AccessExpiresAt, nil
}
func (b *signalBroker) detach(c *signalConnection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.connections[c.principal.ID] == c {
		delete(b.connections, c.principal.ID)
	}
	if route := b.routes[c.principal.HubID]; route != nil && (route.hub == c || route.phone == c) {
		delete(b.routes, c.principal.HubID)
		target := route.hub
		if c == route.hub {
			target = route.phone
		}
		m := route.invite
		m.Type = "peer_closed"
		m.Payload = callsignal.Body(callsignal.Failure{Code: "peer_disconnected"})
		target.send(m)
	}
}
