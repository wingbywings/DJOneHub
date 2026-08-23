package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeviceHubRetainsLogicalDeviceDuringUSBReenumeration(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	physicalID := "usb-2-1.1.1"
	runtime := newDemoApp()
	deviceContext, cancel := context.WithCancel(context.Background())
	hub.devices["device-1"] = &managedUSBDevice{
		ID: "device-1", Alias: "一号", IMEI: "867400000000001", PhysicalID: physicalID,
		State: "ready", Runtime: runtime, Cancel: cancel, Generation: 1,
	}
	hub.physical[physicalID] = "device-1"

	hub.beginPhysicalReconnect(physicalID, "USB identity changed")
	if device := hub.devices["device-1"]; device.State != "reconnecting" || device.Runtime != runtime || device.Generation != 2 {
		t.Fatalf("device was not retained as reconnecting: %#v", device)
	}
	if got := hub.physical[physicalID]; got != "device-1" {
		t.Fatalf("physical mapping = %q, want device-1", got)
	}
	select {
	case <-deviceContext.Done():
		t.Fatal("runtime context was cancelled during the re-enumeration grace period")
	default:
	}

	missingSince := hub.devices["device-1"].MissingSince
	hub.expirePhysicalReconnects(missingSince.Add(usbReenumerationGrace - time.Millisecond))
	if hub.devices["device-1"].State != "reconnecting" {
		t.Fatal("device expired before the re-enumeration grace period")
	}
	hub.expirePhysicalReconnects(missingSince.Add(usbReenumerationGrace))
	if hub.devices["device-1"].State != "offline" {
		t.Fatalf("state = %q, want offline", hub.devices["device-1"].State)
	}
	if _, ok := hub.physical[physicalID]; ok {
		t.Fatal("expired physical mapping was retained")
	}
	select {
	case <-deviceContext.Done():
	default:
		t.Fatal("expired runtime context was not cancelled")
	}
}

func TestAttachErrorKeepsKnownDeviceIdentity(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	locator := usbDeviceLocator{
		VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID,
		Bus: 2, Address: 9, PortPath: []uint8{1, 1, 1},
	}
	physicalID := locator.PhysicalID()
	hub.devices["known-id"] = &managedUSBDevice{
		ID: "known-id", Alias: "保留名称", IMEI: "867400000000001",
		PhysicalID: physicalID, State: "reconnecting", MissingSince: time.Now(),
	}
	hub.physical[physicalID] = "known-id"

	hub.recordAttachError(locator, context.DeadlineExceeded)
	if got := hub.physical[physicalID]; got != "known-id" {
		t.Fatalf("physical mapping = %q, want known-id", got)
	}
	device := hub.devices["known-id"]
	if device.State != "error" || device.Alias != "保留名称" || device.IMEI != "867400000000001" {
		t.Fatalf("known device metadata was replaced: %#v", device)
	}
	if !device.MissingSince.IsZero() {
		t.Fatal("visible-but-not-ready module retained a missing deadline")
	}
}

func TestVoicePreparationReflectsUSBDescriptors(t *testing.T) {
	audio := &usbDeviceStatus{
		LocationID: "0x02111000",
		Interfaces: []usbInterfaceStatus{
			{Number: 2, Class: 1},
			{Number: 6, Class: 255, Subclass: 66},
		},
	}
	ready := voicePreparationFor("ready", audio)
	if ready.State != "usb_audio_available" || !ready.AudioInterface || !ready.ADBInterfaceAdvertised || ready.USBLocationID != audio.LocationID {
		t.Fatalf("unexpected audio preparation: %#v", ready)
	}
	audioOnly := voicePreparationFor("ready", &usbDeviceStatus{Interfaces: []usbInterfaceStatus{{Number: 6, Class: 1, Subclass: 2}}})
	if audioOnly.ADBInterfaceAdvertised {
		t.Fatalf("USB Audio interface 6 was incorrectly advertised as ADB: %#v", audioOnly)
	}

	vendorOnly := voicePreparationFor("ready", &usbDeviceStatus{Interfaces: []usbInterfaceStatus{{Class: 255}}})
	if vendorOnly.State != "module_configuration_required" || vendorOnly.AudioInterface {
		t.Fatalf("unexpected vendor-only preparation: %#v", vendorOnly)
	}

	reconnecting := voicePreparationFor("reconnecting", audio)
	if reconnecting.State != "reconnecting" {
		t.Fatalf("reconnecting preparation: %#v", reconnecting)
	}
}

func TestDeviceHubRoutesRequestToSelectedRuntime(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	first := newDemoApp()
	second := newDemoApp()
	first.sms[0].Content = "from first module"
	second.sms[0].Content = "from second module"
	hub.devices["first"] = &managedUSBDevice{ID: "first", Alias: "一号", State: "ready", Runtime: first, LastSeen: time.Now()}
	hub.devices["second"] = &managedUSBDevice{ID: "second", Alias: "二号", State: "ready", Runtime: second, LastSeen: time.Now()}

	request := httptest.NewRequest(http.MethodGet, "/api/devices/second/sms", nil)
	response := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var messages []receivedSMS
	if err := json.Unmarshal(response.Body.Bytes(), &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(messages) == 0 || messages[0].Content != "from second module" {
		t.Fatalf("request reached wrong runtime: %#v", messages)
	}
}

func TestDeviceHubRoutesDialToSelectedRuntime(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	first := newDemoApp()
	second := newDemoApp()
	hub.devices["first"] = &managedUSBDevice{ID: "first", Alias: "一号", State: "ready", Runtime: first, LastSeen: time.Now()}
	hub.devices["second"] = &managedUSBDevice{ID: "second", Alias: "二号", State: "ready", Runtime: second, LastSeen: time.Now()}

	request := httptest.NewRequest(http.MethodPost, "/api/devices/second/calls/dial", bytes.NewBufferString(`{"number":"10086"}`))
	response := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if first.hasActiveCall() {
		t.Fatal("dial request changed the wrong runtime")
	}
	if !second.callStateAllows("dialing") {
		t.Fatal("dial request did not reach the selected runtime")
	}
}

func TestDeviceHubListsOfflineAndReadyDevices(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	hub.devices["ready"] = &managedUSBDevice{ID: "ready", Alias: "在线", State: "ready", Runtime: newDemoApp()}
	hub.devices["offline"] = &managedUSBDevice{ID: "offline", Alias: "离线", State: "offline"}

	request := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	response := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var devices []usbDeviceSummary
	if err := json.Unmarshal(response.Body.Bytes(), &devices); err != nil {
		t.Fatalf("decode devices: %v", err)
	}
	if len(devices) != 2 || devices[0].ID != "ready" || devices[1].ID != "offline" {
		t.Fatalf("unexpected device order: %#v", devices)
	}
}

func TestRenameDevicePersistsAlias(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	hub.aliasesPath = filepath.Join(t.TempDir(), "aliases.json")
	hub.devices["device-1"] = &managedUSBDevice{ID: "device-1", Alias: "旧名称", State: "ready", Runtime: newDemoApp()}

	request := httptest.NewRequest(http.MethodPatch, "/api/devices/device-1", bytes.NewBufferString(`{"alias":"移动主卡"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	data, err := os.ReadFile(hub.aliasesPath)
	if err != nil {
		t.Fatalf("read aliases: %v", err)
	}
	var aliases map[string]string
	if err := json.Unmarshal(data, &aliases); err != nil {
		t.Fatalf("decode aliases: %v", err)
	}
	if aliases["device-1"] != "移动主卡" {
		t.Fatalf("persisted alias = %q", aliases["device-1"])
	}
}

func TestDeviceHubAllowsOnlyOneNetworkModeOwner(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	first := newDemoApp()
	second := newDemoApp()
	hub.configureNetworkModeGuard(first, "first")
	hub.configureNetworkModeGuard(second, "second")
	hub.devices["first"] = &managedUSBDevice{ID: "first", Alias: "一号", State: "ready", Runtime: first}
	hub.devices["second"] = &managedUSBDevice{ID: "second", Alias: "二号", State: "ready", Runtime: second}

	firstRequest := httptest.NewRequest(http.MethodPost, "/api/devices/first/network/usbnet", bytes.NewBufferString(`{"mode":1}`))
	firstResponse := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(firstResponse, firstRequest)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first module status = %d, body = %s", firstResponse.Code, firstResponse.Body.String())
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/api/devices/second/network/usbnet", bytes.NewBufferString(`{"mode":1}`))
	secondResponse := httptest.NewRecorder()
	hub.routes(webAssets).ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusConflict {
		t.Fatalf("second module status = %d, want 409; body = %s", secondResponse.Code, secondResponse.Body.String())
	}
}

func TestDeviceHubAllowsOnlyOneVoiceRouteOwner(t *testing.T) {
	hub := newUSBDeviceHub()
	defer hub.close()
	first := newDemoApp()
	second := newDemoApp()
	hub.configureVoiceRouteGuard(first, "first")
	hub.configureVoiceRouteGuard(second, "second")
	if err := first.authorizeVoiceRoute(); err != nil {
		t.Fatalf("first voice owner: %v", err)
	}
	if err := second.authorizeVoiceRoute(); err == nil {
		t.Fatal("second module acquired an occupied voice route")
	}
	first.releaseVoiceRoute()
	if err := second.authorizeVoiceRoute(); err != nil {
		t.Fatalf("voice route was not released: %v", err)
	}
}
