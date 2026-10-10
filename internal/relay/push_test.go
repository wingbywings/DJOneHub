//go:build cgo

package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/voippush"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakePush struct {
	calls chan callsignal.Message
	wait  bool
	fail  bool
}

func (f *fakePush) Environment() string { return "sandbox" }
func (f *fakePush) Send(ctx context.Context, token string, m callsignal.Message) (voippush.Result, error) {
	select {
	case f.calls <- m:
	case <-ctx.Done():
		return voippush.Result{}, ctx.Err()
	}
	if f.wait {
		<-ctx.Done()
		return voippush.Result{}, ctx.Err()
	}
	if f.fail {
		return voippush.Result{InvalidToken: true}, voippush.ErrDelivery
	}
	return voippush.Result{}, nil
}
func registerPush(t *testing.T, f *signalFixture, token, env, address string, want int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"environment": env, "token": address})
	req, _ := http.NewRequest("POST", f.http.URL+"/v1/push", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		t.Fatal("register status", res.StatusCode)
	}
}
func TestOfflinePushAttachDeliversOnceAndPreservesBinding(t *testing.T) {
	sender := &fakePush{calls: make(chan callsignal.Message, 4), wait: true}
	f := signalSetup(t, WithPush(sender))
	address := strings.Repeat("a", 64)
	registerPush(t, f, f.phone.AccessToken, "production", address, 503)
	registerPush(t, f, f.hub.AccessToken, "sandbox", address, 401)
	registerPush(t, f, f.phone.AccessToken, "sandbox", address, 200)
	hub := f.dial(t, f.hub)
	m := invitation(hub, f.phone)
	sendSignal(t, hub, m)
	receive(t, hub, "waking")
	select {
	case push := <-sender.calls:
		if !push.SameCall(m) {
			t.Fatal("binding changed")
		}
	case <-time.After(time.Second):
		t.Fatal("no push")
	}
	phone := f.dial(t, f.phone)
	delivered := receive(t, phone, "invite")
	if !delivered.SameCall(m) {
		t.Fatal("wrong invite")
	}
	receive(t, hub, "delivered")
	command := m
	command.Type = "command"
	command.Payload = callsignal.Body(callsignal.Command{Action: "answer", ExpectedRevision: 1})
	sendSignal(t, phone, command)
	receive(t, hub, "command")
	hub.Close()
	receive(t, phone, "peer_closed")
	select {
	case <-sender.calls:
		t.Fatal("duplicate wake")
	default:
	}
}
func TestPendingWakeExpiresAndLatePhoneDoesNotReceiveInvite(t *testing.T) {
	sender := &fakePush{calls: make(chan callsignal.Message, 1)}
	f := signalSetup(t, WithPush(sender))
	registerPush(t, f, f.phone.AccessToken, "sandbox", strings.Repeat("a", 64), 200)
	hub := f.dial(t, f.hub)
	m := invitation(hub, f.phone)
	m.Payload = callsignal.Body(callsignal.Invite{Deadline: time.Now().Add(150 * time.Millisecond), Display: "DJOneHub"})
	sendSignal(t, hub, m)
	receive(t, hub, "waking")
	failure := receive(t, hub, "error")
	if errorCode(t, failure) != "invite_expired" {
		t.Fatal(failure)
	}
	phone := f.dial(t, f.phone)
	f.server.signals.mu.Lock()
	route := f.server.signals.routes[f.hub.Principal.ID]
	f.server.signals.mu.Unlock()
	if route != nil {
		t.Fatal("expired route retained")
	}
	// A new call works; no old invite can precede it on the phone queue.
	next := invitation(hub, f.phone)
	sendSignal(t, hub, next)
	got := receive(t, phone, "invite")
	if !got.SameCall(next) {
		t.Fatal("expired invite replay")
	}
	receive(t, hub, "delivered")
}
func TestPushFailureInvalidatesOnlyTargetAndHubLossClearsPending(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "invalid_token", false: "hub_loss"}[fail], func(t *testing.T) {
			sender := &fakePush{calls: make(chan callsignal.Message, 1), wait: !fail, fail: fail}
			f := signalSetup(t, WithPush(sender))
			registerPush(t, f, f.phone.AccessToken, "sandbox", strings.Repeat("a", 64), 200)
			hub := f.dial(t, f.hub)
			sendSignal(t, hub, invitation(hub, f.phone))
			receive(t, hub, "waking")
			<-sender.calls
			if fail {
				receive(t, hub, "error")
				if _, err := f.store.PushEndpoint(context.Background(), f.hub.AccessToken, f.phone.Principal.ID, "module"); err == nil {
					t.Fatal("bad token retained")
				}
			} else {
				hub.Close()
			}
			f.server.Close() // joins canceled push work without holding broker lock
			f.server.signals.mu.Lock()
			route := f.server.signals.routes[f.hub.Principal.ID]
			f.server.signals.mu.Unlock()
			if route != nil && route.hub.ctx.Err() == nil {
				t.Fatal("pending survived hub loss")
			}
		})
	}
}

func TestMissingOrRevokedPushTargetNeverDispatchesAPNs(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unregistered", true: "revoked"}[revoked], func(t *testing.T) {
			sender := &fakePush{calls: make(chan callsignal.Message, 1)}
			f := signalSetup(t, WithPush(sender))
			if revoked {
				registerPush(t, f, f.phone.AccessToken, "sandbox", strings.Repeat("a", 64), 200)
				if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
					t.Fatal(err)
				}
			}
			hub := f.dial(t, f.hub)
			sendSignal(t, hub, invitation(hub, f.phone))
			receive(t, hub, "waking")
			receive(t, hub, "error")
			select {
			case <-sender.calls:
				t.Fatal("unauthorized target received APNs")
			default:
			}
		})
	}
}
