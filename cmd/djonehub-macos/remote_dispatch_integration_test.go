//go:build cgo && remoteopus && (darwin || linux)

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/remotehub"
	"github.com/iniwex5/vohive/internal/turncred"
)

// Actual production poll -> dispatch -> Session -> Pion offer -> authenticated
// relay path. No hardware is opened, and ringing must never emit ATA by itself.
func TestRemotePollDispatchWSSOpusOfferAndCallerCancellation(t *testing.T) {
	ctx := context.Background()
	database, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	handler := relay.NewServer(database)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	defer handler.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	proof := func(kind string, g identity.Grant) identity.GrantProof {
		pub, key, _ := ed25519.GenerateKey(nil)
		p := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
		p.Signature = ed25519.Sign(key, identity.GrantMessage(kind, p))
		return p
	}
	grant, err := database.ProvisionEnrollment(ctx, "tenant", []string{"dji-test"})
	if err != nil {
		t.Fatal(err)
	}
	hubToken, err := database.Enroll(ctx, proof("enrollment", grant))
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := database.CreatePairing(ctx, hubToken.AccessToken, []string{"dji-test"})
	if err != nil {
		t.Fatal(err)
	}
	phoneToken, err := database.Redeem(ctx, proof("pairing", pairing))
	if err != nil {
		t.Fatal(err)
	}
	config := func(token identity.Tokens) callsignal.ClientConfig {
		return callsignal.ClientConfig{
			URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/v1/signaling", AccessToken: token.AccessToken,
			IdentityID: token.Principal.ID, Kind: token.Principal.Kind, HubID: token.Principal.HubID, TLSConfig: &tls.Config{RootCAs: roots},
		}
	}
	phone, err := callsignal.Dial(ctx, config(phoneToken))
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	manager := remotehub.Start(ctx, remotehub.Config{
		Connect: func(ctx context.Context) (remotehub.Connection, error) {
			c, err := callsignal.Dial(ctx, config(hubToken))
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Refresh: func(context.Context) (string, error) { return "", errors.New("unexpected refresh") },
	})
	defer manager.Close()
	deadline := time.Now().Add(5 * time.Second)
	for manager.Snapshot().State != "online" {
		if time.Now().After(deadline) {
			t.Fatal("connection timeout")
		}
		time.Sleep(time.Millisecond)
	}
	a := remoteTestApp(t)
	a.deviceID = "dji-test"
	var at atomic.Int32
	a.callAT = func(string, time.Duration) (string, error) { at.Add(1); return "", errors.New("unexpected AT") }
	body, _ := json.Marshal(map[string]any{"enabled": true, "target_installation_id": phoneToken.Principal.ID})
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", string(body)); r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	api, err := relayclient.New(server.URL, relayclient.Options{TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	var iceRequests atomic.Int32
	hub := &usbDeviceHub{remoteHub: &remoteHubService{Manager: manager, ice: func(ctx context.Context, module string) (turncred.Credentials, error) {
		iceRequests.Add(1)
		return api.ICE(ctx, hubToken.AccessToken, module)
	}}}
	a.remoteDispatch = func(ctx context.Context, call callRecord) { hub.dispatchRemote(a, ctx, call) }
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "123"}}, time.Now())
	call, ok := a.currentCallIdentity()
	if !ok {
		t.Fatal("missing call")
	}
	gotInvite, gotOffer := false, false
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for !gotInvite || !gotOffer {
		select {
		case message := <-phone.Messages():
			if message.CallUUID != call.UUID || message.ModuleID != "dji-test" || message.Generation != 1 {
				t.Fatal("incorrect binding")
			}
			switch message.Type {
			case "invite":
				gotInvite = true
			case "offer":
				var sdp callsignal.SDP
				if callsignal.Decode(message.Payload, &sdp) != nil || !strings.Contains(sdp.SDP, "opus/48000") {
					t.Fatal("missing Opus offer")
				}
				gotOffer = true
			}
		case <-timeout.C:
			t.Fatal("poll did not produce invitation and Opus offer")
		}
	}
	a.remoteSessionMu.Lock()
	session := a.remoteSession.session
	a.remoteSessionMu.Unlock()
	a.applyCallPoll(nil, time.Now()) // caller hangs up before the phone accepts
	waitSession(t, session)
	closed := false
	timeout.Reset(5 * time.Second)
	for !closed {
		select {
		case message := <-phone.Messages():
			if message.Type == "state" {
				var ended callsignal.State
				if callsignal.Decode(message.Payload, &ended) == nil && ended.State == "ended" {
					if ended.Reason != "cellular_ended" {
						t.Fatal("normal caller hangup lost its reason", ended.Reason)
					}
					closed = true
				}
			}
		case <-timeout.C:
			t.Fatal("phone was not notified of caller cancellation")
		}
	}
	if iceRequests.Load() != 1 {
		t.Fatal("expected one authenticated ICE request per call")
	}
	if at.Load() != 0 {
		t.Fatal("ringing/cancellation sent a modem command")
	}
	if a.remoteCallOwner != nil {
		t.Fatal("offered call leaked ownership")
	}
}

func TestPhoneOutgoingDispatchProducesOpusOfferAndCancelsWithoutAT(t *testing.T) {
	ctx := context.Background()
	database, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	handler := relay.NewServer(database)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	defer handler.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	proof := func(kind string, g identity.Grant) identity.GrantProof {
		pub, key, _ := ed25519.GenerateKey(nil)
		p := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
		p.Signature = ed25519.Sign(key, identity.GrantMessage(kind, p))
		return p
	}
	grant, err := database.ProvisionEnrollment(ctx, "tenant", []string{"dji-test"})
	if err != nil {
		t.Fatal(err)
	}
	hubToken, err := database.Enroll(ctx, proof("enrollment", grant))
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := database.CreatePairing(ctx, hubToken.AccessToken, []string{"dji-test"})
	if err != nil {
		t.Fatal(err)
	}
	phoneToken, err := database.Redeem(ctx, proof("pairing", pairing))
	if err != nil {
		t.Fatal(err)
	}
	config := func(token identity.Tokens) callsignal.ClientConfig {
		return callsignal.ClientConfig{
			URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/v1/signaling", AccessToken: token.AccessToken,
			IdentityID: token.Principal.ID, Kind: token.Principal.Kind, HubID: token.Principal.HubID, TLSConfig: &tls.Config{RootCAs: roots},
		}
	}
	phone, err := callsignal.Dial(ctx, config(phoneToken))
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	var hub *usbDeviceHub
	manager := remotehub.Start(ctx, remotehub.Config{
		Connect: func(ctx context.Context) (remotehub.Connection, error) {
			c, err := callsignal.Dial(ctx, config(hubToken))
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Refresh:  func(context.Context) (string, error) { return "", errors.New("unexpected refresh") },
		Incoming: func(ctx context.Context, c remotehub.Connection, m callsignal.Message) { hub.remoteDial(ctx, c, m) },
	})
	defer manager.Close()
	deadline := time.Now().Add(5 * time.Second)
	for manager.Snapshot().State != "online" {
		if time.Now().After(deadline) {
			t.Fatal("connection timeout")
		}
		time.Sleep(time.Millisecond)
	}
	a := remoteTestApp(t)
	a.deviceID = "dji-test"
	var at atomic.Int32
	a.callAT = func(string, time.Duration) (string, error) { at.Add(1); return "", errors.New("unexpected AT") }
	body, _ := json.Marshal(map[string]any{"enabled": true, "target_installation_id": phoneToken.Principal.ID})
	if r := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", string(body)); r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	api, err := relayclient.New(server.URL, relayclient.Options{TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	var iceRequests atomic.Int32
	hub = &usbDeviceHub{remoteHub: &remoteHubService{Manager: manager, ice: func(ctx context.Context, module string) (turncred.Credentials, error) {
		iceRequests.Add(1)
		return api.ICE(ctx, hubToken.AccessToken, module)
	}}}

	hub.devices = map[string]*managedUSBDevice{"dji-test": {State: "ready", Runtime: a}}
	dial := callsignal.Message{Version: 1, Type: "dial", HubEpoch: phone.Epoch(), ModuleID: "dji-test", CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: phoneToken.Principal.ID, RequestID: callcontrol.NewCallUUID(), Payload: callsignal.Body(callsignal.DialRequest{Number: "12345"})}
	if err := phone.Send(dial); err != nil {
		t.Fatal(err)
	}
	var invite callsignal.Message
	gotOffer, gotAck := false, false
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for invite.Type == "" || !gotOffer || !gotAck {
		select {
		case message, ok := <-phone.Messages():
			if !ok {
				t.Fatal("phone disconnected")
			}
			if message.CallUUID != dial.CallUUID {
				t.Fatal("dial binding changed")
			}
			switch message.Type {
			case "dial_accepted":
				gotAck = true
			case "invite":
				invite = message
			case "offer":
				var offer callsignal.SDP
				_ = callsignal.Decode(message.Payload, &offer)
				if !strings.Contains(offer.SDP, "opus/48000") {
					t.Fatal("missing Opus")
				}
				gotOffer = true
			}
		case <-timeout.C:
			t.Fatal("outgoing did not reach authenticated Hub/Pion offer")
		}
	}
	a.remoteSessionMu.Lock()
	session := a.remoteSession.session
	a.remoteSessionMu.Unlock()
	end := invite
	end.Type = "command"
	end.RequestID = callcontrol.NewCallUUID()
	end.Payload = callsignal.Body(callsignal.Command{Action: "reject", ExpectedRevision: 1})
	if err := phone.Send(end); err != nil {
		t.Fatal(err)
	}
	waitSession(t, session)
	if a.hasActiveCall() || at.Load() != 0 || a.callAudioLease.Generation != 0 {
		t.Fatal("pre-dial cancellation touched modem or retained reservation")
	}
}
