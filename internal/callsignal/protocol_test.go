package callsignal

import (
	"crypto/tls"
	"strings"
	"testing"
)

func TestClientTransportURLPolicy(t *testing.T) {
	for _, tc := range []struct {
		url          string
		insecure, ok bool
	}{
		{"wss://relay.example/v1/signaling", false, true}, {"ws://127.0.0.1:8443/v1/signaling", false, false}, {"ws://127.0.0.1:8443/v1/signaling", true, true}, {"ws://localhost/v1/signaling", true, false}, {"ws://192.0.2.1/v1/signaling", true, false}, {"wss://user:secret@relay.example/v1/signaling", false, false}, {"wss://relay.example/v1/signaling?token=secret", false, false}, {"wss://relay.example/v1/signaling#fragment", false, false}, {"https://relay.example/v1/signaling", false, false},
	} {
		if got := validURL(ClientConfig{URL: tc.url, AllowInsecureLoopback: tc.insecure}); got != tc.ok {
			t.Fatal(tc.url, got)
		}
	}
	if validURL(ClientConfig{URL: "wss://relay.example/v1/signaling", TLSConfig: &tls.Config{InsecureSkipVerify: true}}) {
		t.Fatal("disabled TLS validation")
	}
}

func TestReauthenticationIsNotACallRoutableMessage(t *testing.T) {
	m := Message{Version: 1, Type: "reauth", RequestID: "renewal", Payload: Body(Reauthorize{AccessToken: strings.Repeat("A", 43)})}
	if m.Validate("hub") != nil || m.Validate("installation") != nil {
		t.Fatal("valid reauth rejected")
	}
	m.CallUUID = "00000000-0000-4000-8000-000000000001"
	if m.Validate("hub") == nil {
		t.Fatal("credential associated with a routed call")
	}
	m.CallUUID = ""
	m.Payload = Body(map[string]string{"access_token": strings.Repeat("A", 43), "installation_id": "different"})
	if m.Validate("installation") == nil {
		t.Fatal("reauth accepted a self-reported identity")
	}
}
func TestProtocolRejectsUnknownFieldsAndWrongDirection(t *testing.T) {
	m := Message{Version: 1, Type: "command", HubEpoch: "epoch", ModuleID: "module", CallUUID: "00000000-0000-4000-8000-000000000001", Generation: 1, InstallationID: "phone", RequestID: "request", Payload: Body(Command{Action: "answer", ExpectedRevision: 1})}
	if m.Validate("installation") != nil || m.Validate("hub") == nil {
		t.Fatal("direction constraint")
	}
	m.Payload = []byte(`{"action":"answer","expected_revision":1,"tenant":"other"}`)
	if m.Validate("installation") == nil {
		t.Fatal("unknown command field")
	}
	var out Message
	if Decode([]byte(`{"version":1,"authorization":"secret"}`), &out) == nil {
		t.Fatal("unknown envelope field")
	}
	if Decode([]byte(`{} {}`), &out) == nil {
		t.Fatal("trailing JSON")
	}
	m.Type = "offer"
	m.Payload = Body(SDP{Type: "answer", SDP: "v=0"})
	if m.Validate("hub") == nil {
		t.Fatal("SDP type mismatch")
	}
}
