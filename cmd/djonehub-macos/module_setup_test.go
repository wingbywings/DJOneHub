package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestParseUSBCompositionAndClassification(t *testing.T) {
	factory, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,0`)
	if err != nil || !factory.isFactoryDJI() || factory.hasUAC() || !factory.isRecoverable() {
		t.Fatalf("factory parse = %#v, %v", factory, err)
	}
	full, err := parseUSBComposition("AT+QCFG=\"USBCFG\"\r\n+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,1,1\r\nOK")
	if err != nil || !full.hasUAC() || !full.hasADB() || !full.isUACTarget() {
		t.Fatalf("full UAC parse = %#v, %v", full, err)
	}
	legacy, err := parseUSBComposition(`+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1`)
	if err != nil || !legacy.isLegacyUACTarget() || !legacy.isCallAudioCapable() || legacy.hasADB() {
		t.Fatalf("legacy UAC parse = %#v, %v", legacy, err)
	}
	djiLegacy, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,1`)
	if err != nil || !djiLegacy.isLegacyUACTarget() || !djiLegacy.isCallAudioCapable() {
		t.Fatalf("DJI legacy UAC parse = %#v, %v", djiLegacy, err)
	}
	if got := factory.command(); got != `AT+QCFG="USBCFG",0x2CA3,0x4006,1,1,1,1,1,0,0` {
		t.Fatalf("command = %q", got)
	}
	broken, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,2,0,1,0,1`)
	if err != nil || broken.isRecoverable() {
		t.Fatalf("non-binary configuration accepted: %#v, %v", broken, err)
	}
}

func TestUSBSetupReadbackRequiresADBTargetExactly(t *testing.T) {
	full := usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 1, 1}}
	quectelLegacy := usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}
	djiLegacy := usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}
	factory := usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 0}}
	if usbSetupReadbackMatches(full, quectelLegacy) || usbSetupReadbackMatches(full, djiLegacy) {
		t.Fatal("full voice target must not accept a composition without ADB")
	}
	if !usbSetupReadbackMatches(full, full) {
		t.Fatal("exact full voice target should match")
	}
	if usbSetupReadbackMatches(full, factory) {
		t.Fatal("full UAC request accepted a composition without USB Audio")
	}
	if usbSetupReadbackMatches(factory, djiLegacy) {
		t.Fatal("rollback target must still require an exact readback")
	}
}

func TestQADBChallengeAndPasscodeValidation(t *testing.T) {
	challenge, err := parseQADBChallenge("AT+QADBKEY?\r\n+QADBKEY: 12345678\r\nOK")
	if err != nil || challenge != "12345678" {
		t.Fatalf("challenge = %q, err = %v", challenge, err)
	}
	for _, passcode := range []string{"validKey123", "AbCdEf0123456789"} {
		if err := validateQADBPasscode(passcode); err != nil {
			t.Fatalf("valid passcode rejected: %v", err)
		}
	}
	for _, passcode := range []string{"short", "has-a-dash", "quote\"injection", "line\nbreak"} {
		if err := validateQADBPasscode(passcode); err == nil {
			t.Fatalf("invalid passcode accepted: %q", passcode)
		}
	}
}

func TestTargetVoiceUSBEnablesADBForLegacyUAC(t *testing.T) {
	legacy := usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}
	target := targetVoiceUSB(legacy)
	if !target.isUACTarget() || !target.hasADB() || !target.hasUAC() {
		t.Fatalf("target = %#v", target)
	}
}

func TestParseIMSAndVoLTEConfiguration(t *testing.T) {
	configuration, capability, err := parseIMSConfiguration("AT+QCFG=\"ims\"\r\n+QCFG: \"ims\",1,1\r\nOK")
	if err != nil || configuration != 1 || capability != 1 {
		t.Fatalf("IMS parse = %d,%d err=%v", configuration, capability, err)
	}
	disabled, err := parseVoLTEDisable(`+QCFG: "volte_disable",0`)
	if err != nil || disabled != 0 {
		t.Fatalf("VoLTE parse = %d err=%v", disabled, err)
	}
}

func TestModuleSetupStateClassification(t *testing.T) {
	for _, state := range []string{"initializing", "restarting", "verifying", "rolling_back"} {
		if !moduleSetupIsTransient(state) {
			t.Fatalf("%q should be transient", state)
		}
	}
	for _, state := range []string{"ready", "failed", "rolled_back"} {
		if !moduleSetupIsCachedTerminal(state) {
			t.Fatalf("%q should be cached terminal", state)
		}
	}
	if moduleSetupIsCachedTerminal("configured") {
		t.Fatal("configured must be reinspected so a legacy UAC state can request its QADBKEY passcode")
	}
}

func TestModuleSetupRequiresExplicitConfirmation(t *testing.T) {
	instance := newDemoApp()
	request := httptest.NewRequest(http.MethodPost, "/api/module/setup", bytes.NewBufferString(`{"confirm":false}`))
	response := httptest.NewRecorder()
	instance.moduleSetupStartAPI(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestRolledBackModuleSetupIsReinspected(t *testing.T) {
	instance := newDemoApp()
	instance.moduleSetup = moduleSetupStatus{State: "rolled_back", Summary: "old failure"}
	request := httptest.NewRequest(http.MethodGet, "/api/module/setup", nil)
	response := httptest.NewRecorder()
	instance.moduleSetupStatusAPI(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var status moduleSetupStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.State != "ready" || status.Summary == "old failure" {
		t.Fatalf("rolled-back status was not reinspected: %#v", status)
	}
}

func TestModuleSetupStatePersistsPerDevice(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "voice-setup-state.json")
	first := &app{deviceID: "device-a", moduleSetupPath: path}
	first.setModuleSetup(moduleSetupStatus{TransactionID: "tx-1", State: "restarting", Summary: "restart"})

	second := &app{deviceID: "device-a", moduleSetupPath: path}
	if err := second.loadModuleSetupState(); err != nil {
		t.Fatalf("load setup state: %v", err)
	}
	if second.moduleSetup.TransactionID != "tx-1" || second.moduleSetup.State != "restarting" {
		t.Fatalf("loaded state = %#v", second.moduleSetup)
	}
}

func TestModuleSetupBackupRejectsAnotherDevice(t *testing.T) {
	directory := t.TempDir()
	instance := &app{deviceID: "device-a", moduleSetupBackupDir: directory}
	backup := moduleSetupBackup{
		FormatVersion: 1, DeviceID: "device-a",
		USB: usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 0}},
	}
	path, err := instance.saveModuleSetupBackup("tx-1", backup)
	if err != nil {
		t.Fatalf("save backup: %v", err)
	}
	if _, err := loadModuleSetupBackup(path, "device-b"); err == nil {
		t.Fatal("backup for another device was accepted")
	}
	data, err := json.Marshal(backup)
	if err != nil || len(data) == 0 {
		t.Fatalf("backup is not JSON serializable: %v", err)
	}
}
