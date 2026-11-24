package main

import (
	"context"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/smsout"
	"github.com/iniwex5/vohive/pkg/smscodec"
	"time"
)

// Polling is independent of the call socket; an SMS never consumes a call
// session. Relay claims are durable and are never replayed after uncertainty.
func receiveOutgoingSMS(ctx context.Context, origin string, options relayclient.Options, access func() string, send func(context.Context, smsout.Request) smsout.Result) {
	client, err := relayclient.New(origin, options)
	if err != nil {
		return
	}
	defer client.Close()
	for ctx.Err() == nil {
		request, err := client.ClaimOutgoingSMS(ctx, access())
		if err == nil && request != nil {
			result := send(ctx, *request)
			// Reporting may be retried, executing the SIM submission may not.
			for attempt := 0; attempt < 3; attempt++ {
				report, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err = client.FinishOutgoingSMS(report, access(), result)
				cancel()
				if err == nil || ctx.Err() != nil {
					break
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (h *usbDeviceHub) sendRemoteSMS(ctx context.Context, request smsout.Request) smsout.Result {
	a, ok := h.runtime(request.ModuleID)
	if !ok {
		return smsout.Result{ID: request.ID, InstallationID: request.InstallationID, State: "failed", Code: "module_unavailable"}
	}
	return a.submitRemoteSMS(ctx, request)
}

func (a *app) submitRemoteSMS(ctx context.Context, request smsout.Request) smsout.Result {
	result := smsout.Result{ID: request.ID, InstallationID: request.InstallationID, State: "failed"}
	if code := request.Validate(); code != "" {
		result.Code = code
		return result
	}
	if ctx.Err() != nil {
		result.Code = "module_unavailable"
		return result
	}
	// Do not hold up hardware/call operations while a remote request waits.
	if !a.operationMu.TryLock() {
		result.Code = "module_busy"
		return result
	}
	defer a.operationMu.Unlock()
	a.callRouteMu.Lock()
	allowed := a.appNotificationTarget() == request.InstallationID && request.InstallationID != "" && a.deviceID == request.ModuleID
	busy := a.hasActiveCall() || a.ensureRemoteJournal().status().RecoveryRequired
	a.callRouteMu.Unlock()
	if !allowed {
		result.Code = "sms_not_allowed"
		return result
	}
	if busy {
		result.Code = "module_busy"
		return result
	}
	if ctx.Err() != nil {
		result.Code = "module_unavailable"
		return result
	}
	phone := smsout.NormalizePhone(request.Phone)
	var err error
	if a.smsSubmit != nil {
		result.Segments, err = a.smsSubmit(phone, request.Message)
	} else if a.demo {
		pdus, _, _ := smscodec.BuildSubmitTPDUsWithOptions(phone, request.Message, smsout.SubmitOptions(request.Message))
		result.Segments = len(pdus)
	} else {
		result.Segments, err = a.sendTextSMS(phone, request.Message)
	}
	if err != nil {
		// AT timeouts and partial multipart submissions cannot prove non-delivery.
		result.State, result.Code = "unknown", "submission_uncertain"
		return result
	}
	result.State = "sent"
	a.recordSMS("已发送至 "+phone, request.Message, time.Now())
	return result
}
