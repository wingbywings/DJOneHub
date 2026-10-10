package voippush

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"github.com/iniwex5/vohive/internal/callsignal"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	c, err := New(Config{TeamID: "ABCDEFGHIJ", KeyID: "0123456789", BundleID: "org.djonehub.phone", Environment: "sandbox", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func pushInvite() callsignal.Message {
	return callsignal.Message{Version: 1, Type: "invite", HubEpoch: "epoch", ModuleID: "module", CallUUID: "a885be93-410b-4c59-abed-b7df69a8e61c", Generation: 1, InstallationID: "phone", RequestID: "request", Payload: callsignal.Body(callsignal.Invite{Deadline: time.Now().Add(30 * time.Second), Display: "DJOneHub"})}
}
func TestAPNsHTTP2HeadersJWTAndNoRetries(t *testing.T) {
	c := testClient(t)
	var count atomic.Int32
	var mode atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.ProtoMajor != 2 || r.Method != "POST" || r.URL.Path != "/3/device/"+strings.Repeat("a", 64) || r.Header.Get("apns-expiration") != "0" || r.Header.Get("apns-push-type") != "voip" || r.Header.Get("apns-priority") != "10" || r.Header.Get("apns-topic") != "org.djonehub.phone.voip" || r.Header.Get("apns-id") != pushInvite().CallUUID {
			t.Error("invalid APNs request shape")
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "bearer "), ".")
		if len(parts) != 3 {
			t.Error("bad JWT")
			w.WriteHeader(403)
			return
		}
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if len(signature) != 64 || !ecdsa.Verify(&c.key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
			t.Error("invalid ES256 signature")
		}
		var payload map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 2 || payload["aps"] == nil || payload["invite"] == nil {
			t.Error("unexpected push body")
		}
		switch mode.Load() {
		case 1:
			w.WriteHeader(410)
			w.Write([]byte(`{"reason":"Unregistered","timestamp":1234000}`))
		case 2:
			w.WriteHeader(503)
			w.Write([]byte(`{"reason":"Shutdown"}`))
		default:
			w.WriteHeader(200)
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	c.http = server.Client()
	c.endpoint = server.URL
	for i := 0; i < 3; i++ {
		mode.Store(int32(i))
		result, err := c.Send(context.Background(), strings.Repeat("a", 64), pushInvite())
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i > 0 && err == nil {
			t.Fatal("failure accepted")
		}
		if result.InvalidToken != (i == 1) {
			t.Fatal(result)
		}
		if i == 1 && result.InvalidatedAt.UnixMilli() != 1234000 {
			t.Fatal(result)
		}
	}
	if count.Load() != 3 {
		t.Fatal("retried uncertain delivery")
	}
	expired := pushInvite()
	expired.Payload = callsignal.Body(callsignal.Invite{Deadline: time.Now().Add(-time.Second)})
	if _, err := c.Send(context.Background(), strings.Repeat("a", 64), expired); err == nil || count.Load() != 3 {
		t.Fatal("expired call sent")
	}
}
func TestProviderConfigAndPrivateFile(t *testing.T) {
	c := testClient(t)
	der, _ := x509.MarshalPKCS8PrivateKey(c.key)
	config := c.config
	config.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	path := filepath.Join(t.TempDir(), "apns.json")
	data, _ := json.Marshal(config)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0644)
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("public secret accepted")
	}
	os.Chmod(path, 0600)
	link := path + ".link"
	os.Symlink(path, link)
	if _, err := ReadConfig(link); err == nil {
		t.Fatal("symlink accepted")
	}
	config.Environment = "https://attacker.invalid"
	if _, err := New(config); err == nil {
		t.Fatal("arbitrary endpoint accepted")
	}
	for _, value := range []string{"", strings.Repeat("g", 64), strings.Repeat("a", 1026), "../../token"} {
		if ValidToken(value) {
			t.Fatal("bad token accepted")
		}
	}
}
