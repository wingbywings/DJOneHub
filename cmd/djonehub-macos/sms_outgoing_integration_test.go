//go:build cgo

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/smsout"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhoneHTTPToRelayQueueToHubSIMAndResult(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	proof := func(kind string, g identity.Grant) identity.GrantProof {
		public, key, _ := ed25519.GenerateKey(nil)
		p := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: public}
		p.Signature = ed25519.Sign(key, identity.GrantMessage(kind, p))
		return p
	}
	grant, err := store.ProvisionEnrollment(ctx, "tenant", []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	hubToken, err := store.Enroll(ctx, proof("enrollment", grant))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.CreatePairing(ctx, hubToken.AccessToken, []string{"module"})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := store.Redeem(ctx, proof("pairing", pair))
	if err != nil {
		t.Fatal(err)
	}
	handler := relay.NewServer(store)
	defer handler.Close()
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	request := smsout.Request{ID: uuid.NewString(), ModuleID: "module", Phone: "10086", Message: "端到端测试", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	post := func(token, path string, body any, status int) smsout.Result {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", server.URL+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
		var result smsout.Result
		if status == 200 && path == "/v1/sms/outgoing" {
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	post(phone.AccessToken, "/v1/sms/outgoing/claim", struct{}{}, 401)
	if got := post(phone.AccessToken, "/v1/sms/outgoing", request, 200); got.State != "queued" {
		t.Fatal(got)
	}
	post(phone.AccessToken, "/v1/sms/outgoing", request, 200) // lost-response retry
	a := remoteTestApp(t)
	a.deviceID = "module"
	config, _ := json.Marshal(map[string]any{"enabled": true, "target_installation_id": phone.Principal.ID})
	if w := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", string(config)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var sent atomic.Int32
	a.smsSubmit = func(number, message string) (int, error) {
		sent.Add(1)
		if number != request.Phone || message != request.Message {
			t.Error("content changed")
		}
		return 1, nil
	}
	worker, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveOutgoingSMS(worker, server.URL, relayclient.Options{TLSConfig: &tls.Config{RootCAs: roots}}, func() string { return hubToken.AccessToken }, a.submitRemoteSMS)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, err := store.SubmitSMS(ctx, phone.AccessToken, request)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == "sent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("send did not finish", result)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := post(phone.AccessToken, "/v1/sms/outgoing", request, 200); got.State != "sent" || got.Segments != 1 {
		t.Fatal(got)
	}
	if sent.Load() != 1 {
		t.Fatal("duplicate SIM send", sent.Load())
	}
	post(phone.AccessToken, "/v1/sms/outgoing/result", smsout.Result{ID: request.ID, InstallationID: phone.Principal.ID, State: "sent", Segments: 1}, 401)
}
