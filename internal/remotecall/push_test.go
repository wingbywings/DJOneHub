//go:build cgo

package remotecall

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

func TestWakingAllowsDelayedPhoneWithoutAnsweringOrStartingOffer(t *testing.T) {
	observed := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{Subprotocols: []string{callsignal.Subprotocol}}
		ws, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.WriteJSON(callsignal.Message{Version: 1, Type: "welcome", HubEpoch: "epoch", Payload: callsignal.Body(callsignal.Welcome{ID: "hub", Kind: "hub", HubID: "hub", AccessExpiresAt: time.Now().Add(time.Hour)})})
		var invite callsignal.Message
		if ws.ReadJSON(&invite) != nil {
			return
		}
		waking := invite
		waking.Type = "waking"
		waking.Payload = callsignal.Body(callsignal.Result{Accepted: true})
		ws.WriteJSON(waking)
		// Cross the old five-second online-delivery timeout. There must be no offer
		// or ATA while APNs is merely accepted but the phone has not reconnected.
		time.Sleep(5200 * time.Millisecond)
		waking.Type = "delivered"
		if ws.WriteJSON(waking) != nil {
			return
		}
		for {
			var m callsignal.Message
			if ws.ReadJSON(&m) != nil {
				return
			}
			if m.Type == "offer" {
				observed <- m.Type
				return
			}
		}
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	client, err := callsignal.Dial(ctx, callsignal.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/v1/signaling", AccessToken: strings.Repeat("a", 43), IdentityID: "hub", Kind: "hub", HubID: "hub", TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	config := SignalCall{Config: testConfig(), ModuleID: "module"}
	config.Binding = Binding{CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: "phone"}
	config.Route.TargetInstallationID = "phone"
	config.RingDeadline = time.Now().Add(8 * time.Second)
	backend := &testBackend{}
	media := &signalTestMedia{testMedia: &testMedia{done: make(chan struct{})}, binding: config.Binding, candidates: make(chan remotemedia.Candidate)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSignaledCall(ctx, client, config, func(ctx context.Context) (*Session, SignalMedia, error) {
			s, err := New(ctx, config.Config, backend, func(context.Context) (Media, error) { return media, nil })
			return s, media, err
		})
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("waking did not preserve pending call")
	case <-done:
		select {
		case <-observed:
		default:
			t.Fatal("session ended before phone delivery")
		}
	}
	if backend.answers.Load() != 0 {
		t.Fatal("push acknowledgement answered cellular call")
	}
}
