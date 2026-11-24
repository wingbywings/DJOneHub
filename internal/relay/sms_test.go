//go:build cgo

package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/voippush"
)

type fakeSMSPush struct {
	fakePush
	messages chan voippush.SMS
	invalid  atomic.Bool
}

func (f *fakeSMSPush) SendSMS(ctx context.Context, token string, sms voippush.SMS) (voippush.Result, error) {
	if f.invalid.Load() {
		return voippush.Result{InvalidToken: true}, voippush.ErrDelivery
	}
	f.messages <- sms
	return voippush.Result{}, nil
}

func TestSMSRelayAuthorizationAndIndependentTokenInvalidation(t *testing.T) {
	sender := &fakeSMSPush{messages: make(chan voippush.SMS, 8)}
	f := signalSetup(t, WithPush(sender))
	ctx := context.Background()
	post := func(token, path string, value any, want int) {
		t.Helper()
		data, _ := json.Marshal(value)
		r, _ := http.NewRequest("POST", f.http.URL+path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := f.http.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s status=%d want=%d", path, response.StatusCode, want)
		}
	}
	registration := map[string]string{"environment": "sandbox", "token": strings.Repeat("a", 64)}
	post(f.hub.AccessToken, "/v1/push/alerts", registration, 401)
	post(f.phone.AccessToken, "/v1/push/alerts", registration, 200)
	registerPush(t, f, f.phone.AccessToken, "sandbox", strings.Repeat("b", 64), 200)
	otherHub := f.enroll(t, "foreign")
	foreignPhone := f.pair(t, otherHub, "module")
	sms := voippush.SMS{ID: strings.Repeat("c", 64), ModuleID: "module", HubID: "forged", InstallationID: f.phone.Principal.ID, Sender: "10086", Content: "验证码 1234", Timestamp: time.Now()}
	post(f.phone.AccessToken, "/v1/sms-notifications", sms, 401)
	post(otherHub.AccessToken, "/v1/sms-notifications", sms, 401)
	bad := sms
	bad.ModuleID = "other"
	post(f.hub.AccessToken, "/v1/sms-notifications", bad, 401)
	bad = sms
	bad.InstallationID = foreignPhone.Principal.ID
	post(f.hub.AccessToken, "/v1/sms-notifications", bad, 401)
	client, err := relayclient.New(f.http.URL, relayclient.Options{TLSConfig: f.tls})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SendSMS(ctx, f.hub.AccessToken, sms); err != nil {
		t.Fatal(err)
	}
	got := <-sender.messages
	if got.HubID != f.hub.Principal.ID || got.Content != sms.Content || got.InstallationID != f.phone.Principal.ID {
		t.Fatal("incorrect delivery binding")
	}
	sender.invalid.Store(true)
	post(f.hub.AccessToken, "/v1/sms-notifications", sms, 502)
	if _, err := f.store.AlertPushEndpoint(ctx, f.hub.AccessToken, f.phone.Principal.ID, "module"); err == nil {
		t.Fatal("invalid alert token retained")
	}
	if _, err := f.store.PushEndpoint(ctx, f.hub.AccessToken, f.phone.Principal.ID, "module"); err != nil {
		t.Fatal("alert failure removed VoIP token")
	}
	post(f.phone.AccessToken, "/v1/push/alerts", registration, 200)
	if err := f.store.RevokeInstallation(ctx, f.hub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	post(f.hub.AccessToken, "/v1/sms-notifications", sms, 401)
}
