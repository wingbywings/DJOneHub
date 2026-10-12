package callsignal

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var ErrConnection = errors.New("call signaling connection unavailable")

type ClientConfig struct {
	Resumption              bool     // explicit protocol capability; false preserves v1 fail-closed behavior
	Resume                  *Message // original call binding, never a command to replay
	URL, AccessToken        string
	IdentityID, Kind, HubID string // expected identity, never learned from a caller's frame
	AllowInsecureLoopback   bool
	TLSConfig               *tls.Config // nil uses system roots; never disable verification
}
type Client struct {
	config      ClientConfig
	epoch       string
	ws          *websocket.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	writeMu     sync.Mutex
	in          chan Message
	identity    Welcome
	authMu      sync.Mutex
	renewMu     sync.Mutex
	pending     *renewal
	maintaining bool
}
type renewal struct {
	id   string
	done chan time.Time
}

func validURL(config ClientConfig) bool {
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/v1/signaling" || u.RawPath != "" {
		return false
	}
	if config.TLSConfig != nil && config.TLSConfig.InsecureSkipVerify {
		return false
	}
	if u.Scheme == "wss" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "ws" && config.AllowInsecureLoopback && ip != nil && ip.IsLoopback()
}
func Dial(parent context.Context, config ClientConfig) (*Client, error) {
	if !validURL(config) || len(config.AccessToken) != 43 || !ID(config.IdentityID) || !ID(config.HubID) || (config.Kind != "hub" && config.Kind != "installation") {
		return nil, ErrProtocol
	}
	ctx, cancel := context.WithCancel(parent)
	var tlsConfig *tls.Config
	if config.TLSConfig != nil {
		tlsConfig = config.TLSConfig.Clone()
	}
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{Subprotocol}, TLSClientConfig: tlsConfig}
	headers := http.Header{"Authorization": []string{"Bearer " + config.AccessToken}}
	if config.Resumption {
		headers.Set(RecoveryHeader, "1")
	}
	if config.Resume != nil {
		if !config.Resumption || config.Resume.validateBinding() != nil {
			cancel()
			return nil, ErrProtocol
		}
		m := *config.Resume
		m.Type, m.Payload = "resume", Body(struct{}{})
		headers.Set(ResumeHeader, base64.RawURLEncoding.EncodeToString(Body(m)))
	}
	ws, response, err := d.DialContext(ctx, config.URL, headers)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		cancel()
		return nil, ErrConnection
	}
	c := &Client{config: config, ws: ws, ctx: ctx, cancel: cancel, in: make(chan Message, 8)}
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	ws.SetReadLimit(MaxMessage)
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := ws.ReadMessage()
	var hello Message
	var identity Welcome
	if err != nil || kind != websocket.TextMessage || ws.Subprotocol() != Subprotocol || Decode(data, &hello) != nil || hello.Version != 1 || hello.Type != "welcome" || !ID(hello.HubEpoch) || Decode(hello.Payload, &identity) != nil || identity.ID != config.IdentityID || identity.Kind != config.Kind || identity.HubID != config.HubID || !identity.AccessExpiresAt.After(time.Now()) {
		cancel()
		ws.Close()
		stop()
		return nil, ErrConnection
	}
	if identity.Recovery != config.Resumption || identity.Resumed != (config.Resume != nil) ||
		(config.Resume != nil && config.Kind == "hub" && hello.HubEpoch != config.Resume.HubEpoch) {
		cancel()
		ws.Close()
		stop()
		return nil, ErrConnection
	}
	c.epoch = hello.HubEpoch
	c.identity = identity
	ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	go func() { defer stop(); defer c.Close(); c.read() }()
	go c.heartbeat()
	return c, nil
}
func (c *Client) Epoch() string { return c.epoch }
func (c *Client) Identity() Welcome {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return c.identity
}
func (c *Client) Done() <-chan struct{}    { return c.ctx.Done() }
func (c *Client) Messages() <-chan Message { return c.in }
func (c *Client) Close()                   { c.cancel(); c.ws.Close() }
func (c *Client) Send(m Message) error {
	if m.Type == "reauth" || m.Validate(c.config.Kind) != nil || c.config.Kind == "hub" && m.HubEpoch != c.epoch || c.config.Kind == "installation" && m.InstallationID != c.config.IdentityID {
		return ErrProtocol
	}
	return c.write(m)
}
func (c *Client) write(m Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ctx.Err() != nil {
		return ErrConnection
	}
	c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.ws.WriteJSON(m); err != nil {
		c.Close()
		return ErrConnection
	}
	return nil
}
func (c *Client) read() {
	defer close(c.in)
	for {
		kind, data, err := c.ws.ReadMessage()
		var m Message
		if err != nil || kind != websocket.TextMessage || Decode(data, &m) != nil || m.ValidateDelivery(c.config.Kind) != nil {
			return
		}
		if m.Type == "reauthenticated" {
			var authorization Authorization
			Decode(m.Payload, &authorization)
			c.authMu.Lock()
			pending := c.pending
			if pending == nil || pending.id != m.RequestID || !authorization.AccessExpiresAt.After(time.Now()) {
				c.authMu.Unlock()
				return
			}
			c.identity.AccessExpiresAt = authorization.AccessExpiresAt
			c.pending = nil
			pending.done <- authorization.AccessExpiresAt
			c.authMu.Unlock()
			continue
		}
		if c.config.Kind == "hub" && m.HubEpoch != c.epoch || c.config.Kind == "installation" && m.InstallationID != c.config.IdentityID {
			return
		}
		select {
		case c.in <- m:
		default:
			return
		} // bounded consumer backlog
	}
}

// Renew swaps only the access credential on this connection. The relay checks
// that identity, role, Hub, tenant, version and module scopes stay identical.
// A missing acknowledgement closes the socket rather than guessing its state.
func (c *Client) Renew(ctx context.Context, token string) (time.Time, error) {
	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	m := Message{Version: 1, Type: "reauth", RequestID: uuid.NewString(), Payload: Body(Reauthorize{AccessToken: token})}
	if m.Validate(c.config.Kind) != nil {
		return time.Time{}, ErrProtocol
	}
	pending := &renewal{id: m.RequestID, done: make(chan time.Time, 1)}
	c.authMu.Lock()
	c.pending = pending
	c.authMu.Unlock()
	defer func() {
		c.authMu.Lock()
		if c.pending == pending {
			c.pending = nil
		}
		c.authMu.Unlock()
	}()
	if err := c.write(m); err != nil {
		return time.Time{}, err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case expires := <-pending.done:
		return expires, nil
	case <-ctx.Done():
		c.Close()
		return time.Time{}, ctx.Err()
	case <-c.Done():
		return time.Time{}, ErrConnection
	case <-timer.C:
		c.Close()
		return time.Time{}, ErrConnection
	}
}

// MaintainAuthorization obtains a fresh token one minute before the server's
// expiry. Refresh must serialize and durably save rotated credentials before
// returning. It must honor context; failures end this connection and its call.
// This helper does not own or persist refresh tokens/private keys.
func (c *Client) MaintainAuthorization(parent context.Context, refresh func(context.Context) (string, error)) error {
	if refresh == nil {
		return ErrProtocol
	}
	c.authMu.Lock()
	if c.maintaining {
		c.authMu.Unlock()
		return ErrProtocol
	}
	c.maintaining = true
	c.authMu.Unlock()
	defer func() { c.authMu.Lock(); c.maintaining = false; c.authMu.Unlock() }()
	defer c.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	for {
		expires := c.Identity().AccessExpiresAt
		wait := time.Until(expires.Add(-time.Minute))
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		// Another caller may have renewed while this timer was pending.
		if c.Identity().AccessExpiresAt.After(expires) {
			continue
		}
		deadline := time.Now().Add(30 * time.Second)
		if expires.Before(deadline) {
			deadline = expires
		}
		refreshCtx, stop := context.WithDeadline(ctx, deadline)
		token, err := refresh(refreshCtx)
		if err == nil {
			_, err = c.Renew(refreshCtx, token)
		}
		stop()
		if err != nil {
			return err
		}
		if !c.Identity().AccessExpiresAt.After(time.Now().Add(time.Minute)) {
			return ErrConnection
		} // no busy loop on a non-advancing credential
	}
}
func (c *Client) heartbeat() {
	defer c.Close()
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
		}
	}
}
