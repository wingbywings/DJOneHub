package voippush

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSMSUsesAlertTopicAndBoundsUnicodePayload(t *testing.T) {
	c := testClient(t)
	sms := SMS{ID: strings.Repeat("a", 64), HubID: "hub", ModuleID: "module", InstallationID: "phone", Sender: "10086", Timestamp: time.Now()}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apns-push-type") != "alert" || r.Header.Get("apns-topic") != "org.djonehub.phone" || r.Header.Get("apns-collapse-id") != sms.ID || r.Header.Get("apns-expiration") == "0" {
			t.Error("SMS did not use regular notification headers")
		}
		data, _ := io.ReadAll(r.Body)
		if len(data) > 4096 {
			t.Errorf("payload too large: %d", len(data))
		}
		var payload struct {
			APS struct {
				Alert struct{ Body string }
				Sound string
			}
			SMS    SMS
			Invite any
		}
		if json.Unmarshal(data, &payload) != nil || payload.SMS.ModuleID != sms.ModuleID || payload.Invite != nil || payload.APS.Sound != "default" {
			t.Error("bad SMS payload")
		}
		body := payload.APS.Alert.Body
		if len(sms.Content) < 100 && body != sms.Content {
			t.Error("short content changed")
		}
		if len(sms.Content) > 4096 && (!strings.HasSuffix(body, "…（完整短信请在 Hub 查看）") || strings.ContainsRune(body, '\ufffd')) {
			t.Error("long Unicode content not safely truncated")
		}
		w.WriteHeader(200)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	c.http, c.endpoint = server.Client(), server.URL
	for _, content := range []string{"验证码 1234", strings.Repeat("短信🙂<&", 1800)} {
		sms.Content = content
		if _, err := c.SendSMS(context.Background(), strings.Repeat("b", 64), sms); err != nil {
			t.Fatal(err)
		}
	}
}
