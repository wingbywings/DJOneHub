package main

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/remotehub"
)

// Only the authenticated broker can deliver dial to this idle Hub connection.
// The local module policy is checked again before reserving any hardware.
func (h *usbDeviceHub) remoteDial(ctx context.Context, connection remotehub.Connection, request callsignal.Message) {
	client, ok := connection.(*callsignal.Client)
	if !ok {
		return
	}
	reject := func(code string) {
		m := request
		m.Type = "state"
		m.Payload = callsignal.Body(callsignal.State{Revision: 2, State: "ended", Reason: code})
		_ = client.Send(m)
	}
	var dial callsignal.DialRequest
	if request.Validate("installation") != nil || request.HubEpoch != client.Epoch() || callsignal.Decode(request.Payload, &dial) != nil {
		reject("invalid_dial")
		return
	}
	a, ok := h.runtime(request.ModuleID)
	if !ok {
		reject("module_unavailable")
		return
	}
	if code := a.reserveRemoteDial(request, dial.Number); code != "" {
		reject(code)
		return
	}
	// Release an unclaimed placeholder on ICE/factory failure as well as cancel.
	defer a.discardRemoteDial(request.CallUUID)
	a.callMu.RLock()
	call := *a.activeCall
	a.callMu.RUnlock()
	h.runRemoteCall(ctx, a, call, client)
}

func (a *app) reserveRemoteDial(request callsignal.Message, number string) string {
	if !callsignal.DialNumber(number) {
		return "invalid_number"
	}
	a.callRouteMu.Lock()
	defer a.callRouteMu.Unlock()
	config, loadError := a.ensureRemoteCalls().snapshot()
	agent := a.ensureVoiceAgent().snapshot()
	if loadError != "" || agent.Enabled || !config.Enabled || config.TargetInstallationID != request.InstallationID {
		return "dial_not_allowed"
	}
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if a.activeCall != nil {
		return "module_busy"
	}
	if time.Since(a.lastDialAt) < 2*time.Second {
		return "dial_rate_limited"
	}
	now := time.Now()
	a.lastDialAt = now
	a.activeCall = &callRecord{UUID: request.CallUUID, Revision: 1, ID: request.CallUUID, Direction: "outgoing", State: "dialing", Number: number, StartedAt: now, UpdatedAt: now, ControlOwner: "remote", ControlState: "dial_preparing", Route: callcontrol.SelectRoute(false, 0, config)}
	a.activeCall.Route.RingTimeoutSeconds = 90
	a.ensureCallIdentityLocked()
	return ""
}

func (a *app) discardRemoteDial(uuid string) {
	a.callMu.Lock()
	if a.activeCall == nil || a.activeCall.UUID != uuid || a.activeCall.ControlState != "dial_preparing" {
		a.callMu.Unlock()
		return
	}
	a.activeCall.ControlState = "ending_pending"
	a.callMu.Unlock()
	a.applyCallPoll(nil, time.Now())
}

func (b *remoteCallBackend) dial(ctx context.Context) (bool, error) {
	a, uuid := b.owner.app, b.owner.binding.CallUUID
	if a.prepareCallVoice != nil {
		if err := a.prepareCallVoice(ctx); err != nil {
			return false, err
		}
	} else if !a.demo {
		if err := a.prepareModuleVoiceSessionForCall(ctx, uuid); err != nil {
			return false, err
		}
	}
	if err := lockWithContext(ctx, &a.operationMu); err != nil {
		return false, err
	}
	defer a.operationMu.Unlock()
	a.callMu.RLock()
	valid := a.activeCall != nil && a.activeCall.UUID == uuid && a.activeCall.ControlState == "dial_preparing" && a.remoteCallOwner == b.owner
	number := ""
	if valid {
		number = a.activeCall.Number
	}
	a.callMu.RUnlock()
	if !valid {
		return false, commandError("call_ended", "拨号预约已失效")
	}
	if !a.demo {
		response, err := a.sendCallAT(ctx, "AT+CLCC")
		if err != nil {
			return false, err
		}
		if validateCallATResponse(response) != nil || len(parseCLCC(response)) != 0 {
			return false, commandError("module_busy", "模块已有通话，拒绝拨号")
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	attempted := false
	var err error
	if a.demo {
		attempted = true
	} else {
		var response string
		response, attempted, err = a.sendCallATTracked(ctx, "ATD"+number+";")
		if err == nil && validateCallATResponse(response) != nil {
			attempted = false
			err = commandError("dial_failed", "模块拒绝拨号")
		}
	}
	// An uncertain AT write keeps ownership for bound CLCC/ATH cleanup.
	if attempted {
		a.callMu.Lock()
		if a.activeCall != nil && a.activeCall.UUID == uuid {
			a.activeCall.ControlState = "dial_pending"
			a.activeCall.UpdatedAt = time.Now()
			a.activeCall.Revision++
		}
		a.callMu.Unlock()
	}
	if a.demo {
		a.applyCallPoll([]parsedCall{{Index: 1, Direction: "outgoing", State: "active", Number: number}}, time.Now())
	}
	return attempted, err
}

func (b *remoteCallBackend) reconcileOutgoing(ctx context.Context) error {
	a := b.owner.app
	a.callMu.RLock()
	outgoing := a.activeCall != nil && a.activeCall.UUID == b.owner.binding.CallUUID && a.activeCall.Direction == "outgoing"
	a.callMu.RUnlock()
	if !outgoing || a.demo {
		return nil
	}
	if err := lockWithContext(ctx, &a.operationMu); err != nil {
		return err
	}
	defer a.operationMu.Unlock()
	a.callMu.RLock()
	stillOwned := a.activeCall != nil && a.activeCall.UUID == b.owner.binding.CallUUID
	a.callMu.RUnlock()
	if !stillOwned {
		return nil
	}
	response, err := a.sendCallAT(ctx, "AT+CLCC")
	if err != nil {
		return err
	}
	if err := validateCallATResponse(response); err != nil {
		return err
	}
	calls := parseCLCC(response)
	if len(calls) > 1 {
		return commandError("ambiguous_call", "无法确认原拨号通话")
	}
	// No call is positive teardown evidence, bypassing only the polling grace.
	if len(calls) == 0 {
		a.callMu.Lock()
		if a.activeCall != nil && a.activeCall.UUID == b.owner.binding.CallUUID {
			a.activeCall.ControlState = "ending_pending"
		}
		a.callMu.Unlock()
	}
	a.applyCallPoll(calls, time.Now())
	return nil
}
