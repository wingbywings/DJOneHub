//go:build cgo

package relay

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotehub"
)

func TestHubManagerWSSDisconnectRetiresEpochWithoutReplayingCall(t *testing.T) {
	f := signalSetup(t)
	phone := f.dial(t, f.phone)
	connections := make(chan *callsignal.Client, 4)
	var count atomic.Int32
	m := remotehub.Start(context.Background(), remotehub.Config{
		Connect: func(ctx context.Context) (remotehub.Connection, error) {
			c, err := callsignal.Dial(ctx, f.config(f.hub))
			if err != nil {
				return nil, err
			}
			count.Add(1)
			connections <- c
			return c, nil
		},
		Refresh: func(context.Context) (string, error) { return "", errors.New("unexpected early renewal") },
	})
	defer m.Close()
	online := func() {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for m.Snapshot().State != "online" {
			if time.Now().After(deadline) {
				t.Fatal("manager did not connect", m.Snapshot())
			}
			time.Sleep(time.Millisecond)
		}
	}
	getConnection := func() *callsignal.Client {
		t.Helper()
		select {
		case c := <-connections:
			return c
		case <-time.After(6 * time.Second):
			t.Fatal("connection timeout")
		}
		return nil
	}
	first := getConnection()
	online()
	errorsOut := make(chan error, 2)
	run := func(ctx context.Context, connection remotehub.Connection) {
		c := connection.(*callsignal.Client)
		if err := c.Send(invitation(c, f.phone)); err != nil {
			errorsOut <- err
			return
		}
		select {
		case message := <-c.Messages():
			if message.Type != "delivered" {
				errorsOut <- errors.New("invite not delivered")
				return
			}
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}
	if !m.TryStart(context.Background(), run) {
		t.Fatal("first call rejected")
	}
	firstInvite := receive(t, phone, "invite")
	first.Close()
	receive(t, phone, "peer_closed")
	second := getConnection()
	online()
	if second.Epoch() == first.Epoch() || count.Load() != 2 {
		t.Fatal("epoch was reused")
	}
	select {
	case message := <-phone.Messages():
		t.Fatalf("old invitation replayed: %+v", message)
	case <-time.After(100 * time.Millisecond):
	}
	if !m.TryStart(context.Background(), run) {
		t.Fatal("new call rejected")
	}
	secondInvite := receive(t, phone, "invite")
	if firstInvite.CallUUID == secondInvite.CallUUID || secondInvite.HubEpoch != second.Epoch() {
		t.Fatal("new call retained old binding")
	}
	m.Close()
	receive(t, phone, "peer_closed")
	select {
	case err := <-errorsOut:
		t.Fatal(err)
	default:
	}
}
