package main

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/iniwex5/vohive/internal/smsout"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRemoteSMSScopeBusyPartialFailureAndSentHistory(t *testing.T) {
	a := remoteTestApp(t)
	a.deviceID = "module"
	request := smsout.Request{ID: uuid.NewString(), ModuleID: "module", InstallationID: "phone-1", Phone: "10086", Message: strings.Repeat("中", 71), CreatedAt: time.Now()}
	if got := a.submitRemoteSMS(context.Background(), request); got.Code != "sms_not_allowed" {
		t.Fatal(got)
	}
	if w := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	wrong := request
	wrong.InstallationID = "phone-2"
	if got := a.submitRemoteSMS(context.Background(), wrong); got.Code != "sms_not_allowed" {
		t.Fatal(got)
	}
	a.operationMu.Lock()
	if got := a.submitRemoteSMS(context.Background(), request); got.Code != "module_busy" {
		t.Fatal(got)
	}
	a.operationMu.Unlock()
	a.activeCall = &callRecord{UUID: uuid.NewString()}
	if got := a.submitRemoteSMS(context.Background(), request); got.Code != "module_busy" {
		t.Fatal(got)
	}
	a.activeCall = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := a.submitRemoteSMS(ctx, request); got.Code != "module_unavailable" {
		t.Fatal(got)
	}
	var sends int
	a.smsSubmit = func(phone, message string) (int, error) { sends++; return 1, errors.New("second segment timed out") }
	if got := a.submitRemoteSMS(context.Background(), request); got.State != "unknown" || got.Segments != 1 || sends != 1 {
		t.Fatal(got, sends)
	}
	if len(a.sms) != 0 {
		t.Fatal("partial failure reported as sent")
	}
	a.smsSubmit = nil
	if got := a.submitRemoteSMS(context.Background(), request); got.State != "sent" || got.Segments != 2 {
		t.Fatal(got)
	}
	if len(a.sms) != 1 || a.sms[0].Sender != "已发送至 10086" || a.sms[0].Content != request.Message {
		t.Fatal("missing sent history")
	}
}

func TestRemoteSMSNormalizesLegacyPhoneAtHardwareBoundary(t *testing.T) {
	a := remoteTestApp(t)
	a.deviceID = "module"
	if w := remoteRequest(a, http.MethodPut, "/api/remote-calls/config", enabledRemoteBody); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	request := smsout.Request{ID: uuid.NewString(), ModuleID: "module", InstallationID: "phone-1", Phone: "13800138000", Message: "测试", CreatedAt: time.Now()}
	digest := request.Digest()
	a.smsSubmit = func(phone, message string) (int, error) {
		if phone != "+8613800138000" {
			t.Errorf("hardware phone = %q", phone)
		}
		return 1, nil
	}
	if got := a.submitRemoteSMS(context.Background(), request); got.State != "sent" {
		t.Fatal(got)
	}
	if request.Digest() != digest {
		t.Fatal("original retry digest changed")
	}
	if len(a.sms) != 1 || a.sms[0].Sender != "已发送至 +8613800138000" {
		t.Fatal("sent history lost actual destination")
	}
}
