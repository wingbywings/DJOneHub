//go:build cgo

package relay

import (
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"testing"
	"time"
)

func TestOutgoingRouteBindsHubEpochAndAllowsCancelBeforeInvite(t *testing.T) {
	f := signalSetup(t)
	phone := f.dial(t, f.phone)
	hub := f.dial(t, f.hub)
	dial := callsignal.Message{Version: 1, Type: "dial", HubEpoch: phone.Epoch(), ModuleID: "module", CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: f.phone.Principal.ID, RequestID: callcontrol.NewCallUUID(), Payload: callsignal.Body(callsignal.DialRequest{Number: "+8613800000000"})}
	sendSignal(t, phone, dial)
	routed := receive(t, hub, "dial")
	ack := receive(t, phone, "dial_accepted")
	if routed.HubEpoch != hub.Epoch() || !routed.SameCall(ack) {
		t.Fatal("dial did not bind current Hub epoch")
	}
	cancel := dial
	cancel.Type = "cancel_dial"
	cancel.RequestID = callcontrol.NewCallUUID()
	cancel.Payload = callsignal.Body(struct{}{})
	sendSignal(t, phone, cancel)
	command := receive(t, hub, "command")
	var payload callsignal.Command
	_ = callsignal.Decode(command.Payload, &payload)
	if !command.SameCall(routed) || payload.Action != "reject" || payload.ExpectedRevision != 1 {
		t.Fatal("pending cancel did not target exact dial")
	}
	invite := routed
	invite.Type = "invite"
	invite.RequestID = callcontrol.NewCallUUID()
	invite.Payload = callsignal.Body(callsignal.Invite{Deadline: time.Now().Add(60 * time.Second), Display: "dial", Direction: "outgoing"})
	sendSignal(t, hub, invite)
	receive(t, phone, "invite")
	receive(t, hub, "delivered")
	ended := invite
	ended.Type = "state"
	ended.Payload = callsignal.Body(callsignal.State{State: "ended", Revision: 2, Reason: "rejected"})
	sendSignal(t, hub, ended)
	receive(t, phone, "state")
	sendSignal(t, phone, dial)
	if errorCode(t, receive(t, phone, "error")) != "call_unavailable" {
		t.Fatal("dial replay allowed")
	}
}

func TestOutgoingBusyOfflineAndForgedInstallationNeverReachHub(t *testing.T) {
	f := signalSetup(t)
	phone := f.dial(t, f.phone)
	dial := callsignal.Message{Version: 1, Type: "dial", HubEpoch: phone.Epoch(), ModuleID: "module", CallUUID: callcontrol.NewCallUUID(), Generation: 1, InstallationID: f.phone.Principal.ID, RequestID: callcontrol.NewCallUUID(), Payload: callsignal.Body(callsignal.DialRequest{Number: "12345"})}
	sendSignal(t, phone, dial)
	if errorCode(t, receive(t, phone, "error")) != "target_unavailable" {
		t.Fatal("offline dial was queued")
	}
	hub := f.dial(t, f.hub)
	sendSignal(t, phone, dial)
	receive(t, hub, "dial")
	receive(t, phone, "dial_accepted")
	other := dial
	other.CallUUID = callcontrol.NewCallUUID()
	other.RequestID = callcontrol.NewCallUUID()
	sendSignal(t, phone, other)
	if errorCode(t, receive(t, phone, "error")) != "call_unavailable" {
		t.Fatal("second call routed")
	}
	other.InstallationID = "forged"
	// Send raw to exercise broker identity checks beyond the client fence.
	if err := phone.Send(other); err != callsignal.ErrProtocol {
		t.Fatal("client accepted forged installation", err)
	}
}
