package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
