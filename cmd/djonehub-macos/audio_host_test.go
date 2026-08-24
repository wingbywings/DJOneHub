package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAudioHostConfigUsesExactDeviceLocator(t *testing.T) {
	locator := usbDeviceLocator{
		VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID,
		Bus: 2, Address: 9, PortPath: []uint8{1, 1, 3}, LocationID: 0x02113000,
	}
	instance := &app{deviceID: "device-3", usbLocator: &locator, moduleVoiceReady: true}
	instance.activeCall = &callRecord{ID: "call-1", State: "active", StartedAt: time.Now()}
	instance.audioHost.WantMuted = true

	request := httptest.NewRequest(http.MethodGet, "/api/calls/audio/host/config", nil)
	response := httptest.NewRecorder()
	instance.audioHostConfig(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var config audioHostConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if config.DeviceID != "device-3" || config.LocationID != 0x02113000 || config.VendorID != quectelUSBVendorID || !config.RouteReady || !config.CallActive || !config.Muted {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestAudioHostConfigRejectsMissingLocationID(t *testing.T) {
	locator := usbDeviceLocator{VendorID: djiUSBVendorID, ProductID: djiUSBProductID}
	instance := &app{usbLocator: &locator}
	response := httptest.NewRecorder()
	instance.audioHostConfig(response, httptest.NewRequest(http.MethodGet, "/api/calls/audio/host/config", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
}

func TestAudioHostMuteAndRecordRequireActiveCall(t *testing.T) {
	instance := &app{}
	for path, body := range map[string]string{
		"/api/calls/audio/mute":   `{"muted":true}`,
		"/api/calls/audio/record": `{"enabled":true}`,
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		instance.routes().ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("%s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}

	instance.activeCall = &callRecord{ID: "call-1", State: "active", StartedAt: time.Now()}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/calls/audio/mute", bytes.NewBufferString(`{"muted":true}`))
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !instance.audioHost.WantMuted {
		t.Fatalf("active mute status = %d, state = %#v", response.Code, instance.audioHost)
	}
}

func TestAudioHostRegistrationIsReportedInCallStatus(t *testing.T) {
	instance := newDemoApp()
	register := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/calls/audio/host/register", bytes.NewBufferString(`{"enabled":true,"running":true}`))
	instance.routes().ServeHTTP(register, request)
	if register.Code != http.StatusOK {
		t.Fatalf("register status = %d", register.Code)
	}
	if state := instance.audioHostSnapshot(); !state.Registered || !state.Running {
		t.Fatalf("audio host state = %#v", state)
	}
}

func TestAudioHostCannotStopRequiredAIRecording(t *testing.T) {
	instance := newDemoApp()
	now := time.Now()
	instance.activeCall = &callRecord{ID: "call-ai-recording", Direction: "incoming", State: "active", AIHandled: true, StartedAt: now, UpdatedAt: now}
	instance.audioHost.WantRecording = true
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/calls/audio/record", bytes.NewBufferString(`{"enabled":false}`))
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !instance.audioHost.WantRecording {
		t.Fatal("required AI recording intent was cleared")
	}
}
