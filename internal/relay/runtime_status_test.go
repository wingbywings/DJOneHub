//go:build cgo

package relay

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/turncred"
)

func TestManagedPresenceUsesLiveWSSAndHubScope(t *testing.T) {
	f := signalSetup(t, WithBuild("presence-test"))
	otherHub := f.enroll(t, "other-tenant")
	otherPhone := f.pair(t, otherHub, "module")
	phone := f.dial(t, f.phone)
	f.dial(t, otherPhone)
	read := func() []identity.ManagedIdentity {
		r := httptest.NewRequest("GET", "/v1/management/installations", nil)
		r.Header.Set("Authorization", "Bearer "+f.hub.AccessToken)
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), otherPhone.Principal.ID) || strings.Contains(w.Body.String(), f.phone.AccessToken) {
			t.Fatal("presence leaked identity or credential")
		}
		var body struct {
			Installations []identity.ManagedIdentity `json:"installations"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Installations) != 1 {
			t.Fatal("unexpected installation list")
		}
		return body.Installations
	}
	deadline := time.Now().Add(3 * time.Second)
	for read()[0].SignalState != "online" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if read()[0].SignalState != "online" {
		t.Fatal("live WSS not observed")
	}
	s := f.server.RuntimeSnapshot(context.Background())
	if !s.Running || !s.IdentityHealthy || s.Build != "presence-test" || s.OnlineCount != 2 || s.APNs.Loaded || s.TURN.Loaded {
		t.Fatal("runtime snapshot incorrect", s)
	}
	phone.Close()
	deadline = time.Now().Add(3 * time.Second)
	for read()[0].SignalState != "offline" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if read()[0].SignalState != "offline" {
		t.Fatal("disconnected WSS retained online state")
	}
	f.dial(t, f.phone)
	if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if item := read()[0]; !item.Revoked || item.SignalState != "offline" {
		t.Fatal("revoked socket reported online")
	}
}

func TestRuntimeReflectsObservedPushAndTURNRequestsWithoutSecrets(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "failed"}[failed], func(t *testing.T) {
			sender := &fakePush{calls: make(chan callsignal.Message, 1), fail: failed}
			secret := strings.Repeat("s", 43)
			issuer, err := turncred.New(turncred.Config{URLs: []string{"turn:relay.example:3478?transport=udp"}, Secret: secret, RelayOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			f := signalSetup(t, WithPush(sender), WithTURN(issuer))
			address := strings.Repeat("a", 64)
			registerPush(t, f, f.phone.AccessToken, "sandbox", address, 200)
			if request(f.server, "POST", "/v1/ice", f.phone.AccessToken, map[string]string{"module_id": "module"}).Code != 200 {
				t.Fatal("TURN issue")
			}
			hub := f.dial(t, f.hub)
			invite := invitation(hub, f.phone)
			sendSignal(t, hub, invite)
			receive(t, hub, "waking")
			deadline := time.Now().Add(3 * time.Second)
			s := f.server.RuntimeSnapshot(context.Background())
			for s.APNs.LastAttempt == nil && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				s = f.server.RuntimeSnapshot(context.Background())
			}
			if s.APNs.LastAttempt == nil || s.APNs.LastResult != map[bool]string{false: "accepted", true: "failed"}[failed] || s.APNs.Environment != "sandbox" || s.TURN.Successes != 1 || s.TURN.LastResult != "accepted" {
				t.Fatal("unobserved activity", s)
			}
			data, _ := json.Marshal(s)
			for _, private := range []string{secret, address, f.phone.AccessToken, f.hub.AccessToken, invite.CallUUID} {
				if strings.Contains(string(data), private) {
					t.Fatal("runtime status leaked private call/provider data")
				}
			}
		})
	}
}
