//go:build cgo

package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
)

type signalFixture struct {
	store      *identity.Store
	server     *Server
	http       *httptest.Server
	tls        *tls.Config
	hub, phone identity.Tokens
	directory  string
	keys       map[string]ed25519.PrivateKey
}

func signalSetup(t *testing.T, options ...Option) *signalFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "state")
	s, err := identity.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := NewServer(s, options...)
	httpServer := httptest.NewTLSServer(h)
	roots := x509.NewCertPool()
	roots.AddCert(httpServer.Certificate())
	f := &signalFixture{store: s, server: h, http: httpServer, tls: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, directory: directory, keys: make(map[string]ed25519.PrivateKey)}
	f.hub = f.enroll(t, "tenant")
	f.phone = f.pair(t, f.hub, "module")
	t.Cleanup(func() { h.Close(); httpServer.Close() })
	return f
}
func (f *signalFixture) enroll(t *testing.T, tenant string) identity.Tokens {
	t.Helper()
	g, err := f.store.ProvisionEnrollment(context.Background(), tenant, []string{"module", "other"})
	if err != nil {
		t.Fatal(err)
	}
	p, key := signed("enrollment", g)
	token, err := f.store.Enroll(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.keys[token.Principal.ID] = key
	return token
}
func (f *signalFixture) pair(t *testing.T, hub identity.Tokens, module string) identity.Tokens {
	t.Helper()
	g, err := f.store.CreatePairing(context.Background(), hub.AccessToken, []string{module})
	if err != nil {
		t.Fatal(err)
	}
	p, key := signed("pairing", g)
	token, err := f.store.Redeem(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.keys[token.Principal.ID] = key
	return token
}
func (f *signalFixture) config(token identity.Tokens) callsignal.ClientConfig {
	return callsignal.ClientConfig{URL: "wss" + strings.TrimPrefix(f.http.URL, "https") + "/v1/signaling", AccessToken: token.AccessToken, IdentityID: token.Principal.ID, Kind: token.Principal.Kind, HubID: token.Principal.HubID, TLSConfig: f.tls}
}
func (f *signalFixture) dial(t *testing.T, token identity.Tokens) *callsignal.Client {
	t.Helper()
	c, err := callsignal.Dial(context.Background(), f.config(token))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func receive(t *testing.T, c *callsignal.Client, kind string) callsignal.Message {
	t.Helper()
	select {
	case m, ok := <-c.Messages():
		if !ok || m.Type != kind {
			t.Fatalf("want %s got %+v open=%v", kind, m, ok)
		}
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("waiting for %s", kind)
	}
	return callsignal.Message{}
}
func sendSignal(t *testing.T, c *callsignal.Client, m callsignal.Message) {
	t.Helper()
	if err := c.Send(m); err != nil {
		t.Fatal(err)
	}
}
func invitation(hub *callsignal.Client, phone identity.Tokens) callsignal.Message {
	return callsignal.Message{Version: 1, Type: "invite", HubEpoch: hub.Epoch(), ModuleID: "module", CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: phone.Principal.ID, RequestID: callcontrol.NewCallUUID(), Payload: callsignal.Body(callsignal.Invite{Deadline: time.Now().Add(30 * time.Second), Display: "test"})}
}
func errorCode(t *testing.T, m callsignal.Message) string {
	t.Helper()
	var failure callsignal.Failure
	if callsignal.Decode(m.Payload, &failure) != nil {
		t.Fatal("invalid error")
	}
	return failure.Code
}

func TestWSSOnlineRoutingAndCallFences(t *testing.T) {
	f := signalSetup(t)
	phone := f.dial(t, f.phone)
	hub := f.dial(t, f.hub)
	invite := invitation(hub, f.phone)
	sendSignal(t, hub, invite)
	receive(t, phone, "invite")
	receive(t, hub, "delivered")
	command := invite
	command.Type = "command"
	command.RequestID = callcontrol.NewCallUUID()
	command.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	wrong := command
	wrong.Generation++
	sendSignal(t, phone, wrong)
	if code := errorCode(t, receive(t, phone, "error")); code != "call_unavailable" {
		t.Fatal(code)
	}
	sendSignal(t, phone, command)
	got := receive(t, hub, "command")
	if got.RequestID != command.RequestID || got.InstallationID != f.phone.Principal.ID {
		t.Fatal("identity changed")
	}
	// ICE and SDP are not allowed to select a different live call.
	offer := invite
	offer.Type = "offer"
	offer.Payload = callsignal.Body(callsignal.SDP{Type: "offer", SDP: "v=0\r\n"})
	sendSignal(t, hub, offer)
	receive(t, phone, "offer")
	ended := invite
	ended.Type = "state"
	ended.Payload = callsignal.Body(callsignal.State{Revision: 3, State: "ended", Reason: "cancelled"})
	sendSignal(t, hub, ended)
	receive(t, phone, "state")
	sendSignal(t, phone, command)
	receive(t, phone, "error")
	sendSignal(t, hub, invite)
	if errorCode(t, receive(t, hub, "error")) != "call_unavailable" {
		t.Fatal("ended UUID reused")
	}
}
func TestWSSRejectsCrossHubTargetAndWrongInstallation(t *testing.T) {
	f := signalSetup(t)
	hub := f.dial(t, f.hub)
	phone := f.dial(t, f.phone)
	otherHub := f.enroll(t, "tenant")
	otherPhone := f.pair(t, otherHub, "module")
	f.dial(t, otherPhone)
	invite := invitation(hub, otherPhone)
	sendSignal(t, hub, invite)
	if errorCode(t, receive(t, hub, "error")) != "target_unavailable" {
		t.Fatal("cross-Hub invitation")
	}
	limited := f.pair(t, f.hub, "other")
	f.dial(t, limited)
	invite = invitation(hub, limited)
	sendSignal(t, hub, invite)
	receive(t, hub, "error")
	invite = invitation(hub, f.phone)
	sendSignal(t, hub, invite)
	receive(t, hub, "delivered")
	receive(t, phone, "invite")
	intruderToken := f.pair(t, f.hub, "module")
	intruder := f.dial(t, intruderToken)
	command := invite
	command.InstallationID = intruderToken.Principal.ID
	command.Type = "command"
	command.Payload = callsignal.Body(callsignal.Command{Action: "hangup", ExpectedRevision: 1})
	sendSignal(t, intruder, command)
	receive(t, intruder, "error")
}
func TestWSSRevocationClosesSocketAndCancelsRoute(t *testing.T) {
	f := signalSetup(t)
	hub := f.dial(t, f.hub)
	phone := f.dial(t, f.phone)
	m := invitation(hub, f.phone)
	sendSignal(t, hub, m)
	receive(t, hub, "delivered")
	receive(t, phone, "invite")
	if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-phone.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("revoked socket alive")
	}
	receive(t, hub, "peer_closed")
	if c, err := callsignal.Dial(context.Background(), f.config(f.phone)); err == nil {
		c.Close()
		t.Fatal("revoked token reconnected")
	}
}
func TestWSSReconnectChangesEpochAndShutdownClosesSockets(t *testing.T) {
	f := signalSetup(t)
	hub := f.dial(t, f.hub)
	phone := f.dial(t, f.phone)
	old := invitation(hub, f.phone)
	sendSignal(t, hub, old)
	receive(t, hub, "delivered")
	receive(t, phone, "invite")
	hub.Close()
	receive(t, phone, "peer_closed")
	hub = f.dial(t, f.hub)
	if hub.Epoch() == old.HubEpoch {
		t.Fatal("epoch reused")
	}
	// Phone frames still carry the old Hub epoch, regardless of its own socket.
	old.Type = "command"
	old.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	sendSignal(t, phone, old)
	receive(t, phone, "error")
	f.server.Close()
	for _, c := range []*callsignal.Client{hub, phone} {
		select {
		case <-c.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown left websocket open")
		}
	}
}
func TestWSSHandshakeAndFrameBoundaries(t *testing.T) {
	f := signalSetup(t)
	cfg := f.config(f.hub)
	for _, tc := range []struct {
		query, origin, token, protocol string
		status                         int
	}{
		{"", "", "", callsignal.Subprotocol, 401}, {"?token=x", "", cfg.AccessToken, callsignal.Subprotocol, 400}, {"", "https://evil.example", cfg.AccessToken, callsignal.Subprotocol, 403}, {"", "", cfg.AccessToken, "other", 400},
	} {
		d := websocket.Dialer{TLSClientConfig: f.tls, Subprotocols: []string{tc.protocol}}
		h := http.Header{"Authorization": []string{"Bearer " + tc.token}}
		if tc.origin != "" {
			h.Set("Origin", tc.origin)
		}
		c, r, err := d.Dial(cfg.URL+tc.query, h)
		if c != nil {
			c.Close()
		}
		if r != nil {
			r.Body.Close()
		}
		if err == nil || r == nil || r.StatusCode != tc.status {
			t.Fatalf("handshake status: %v %v", r, err)
		}
	}
	hub := f.dial(t, f.hub)
	if c, err := callsignal.Dial(context.Background(), cfg); err == nil {
		c.Close()
		t.Fatal("second socket took identity")
	}
	hub.Close()
	// Wait until server releases identity before opening raw adversarial socket.
	deadline := time.Now().Add(time.Second)
	for {
		f.server.signals.mu.Lock()
		empty := f.server.signals.connections[f.hub.Principal.ID] == nil
		f.server.signals.mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detach timeout")
		}
		time.Sleep(time.Millisecond)
	}
	d := websocket.Dialer{TLSClientConfig: f.tls, Subprotocols: []string{callsignal.Subprotocol}}
	c, _, err := d.Dial(cfg.URL, http.Header{"Authorization": []string{"Bearer " + cfg.AccessToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", callsignal.MaxMessage+1))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("oversize frame accepted")
	}
}
