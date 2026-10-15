package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if challenge, err := parseQADBChallenge("+QADBKEY: 123456789\r\nOK"); err == nil {
		t.Fatalf("overlong challenge accepted as %q", challenge)
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

func TestGenerateQADBPasscodeMatchesPasslibReference(t *testing.T) {
	tests := map[string]string{
		"1":        "JkxHEjF8W4v1cb8",
		"12":       "LE4IjfdhEbTYf/p",
		"1234567":  "0xxRaPk.6wC7A5F",
		"87449288": "pZ4SRpJVIqrVJv5",
		"12345678": "0jXKXQwSwMxYoeg",
		"00000000": "C3SV/fMl8Tu3SeK",
		"99999999": "2ajrfuUYlCCWbY3",
	}
	for challenge, want := range tests {
		got, err := generateQADBPasscode(challenge)
		if err != nil || got != want {
			t.Errorf("generateQADBPasscode(%q) = %q, %v; want %q", challenge, got, err, want)
		}
	}
	for _, challenge := range []string{"", "123456789", "1234abcd", "12/34"} {
		if passcode, err := generateQADBPasscode(challenge); err == nil {
			t.Errorf("invalid challenge %q generated passcode %q", challenge, passcode)
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

func TestADBUnlockDetectionCoversFactoryAndLegacyLayouts(t *testing.T) {
	tests := []struct {
		name string
		usb  usbComposition
		want bool
	}{
		{
			name: "DJI factory layout",
			usb:  usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 0}},
			want: true,
		},
		{
			name: "DJI legacy UAC layout",
			usb:  usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}},
			want: true,
		},
		{
			name: "Quectel legacy UAC layout",
			usb:  usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}},
			want: true,
		},
		{
			name: "full voice target",
			usb:  usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 1, 1}},
			want: false,
		},
		{
			name: "unsupported USB identity",
			usb:  usbComposition{VendorID: 0x1234, ProductID: 0x5678, Flags: []int{1, 1, 1, 1, 1, 0, 0}},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.usb.needsADBUnlockForVoice(); got != test.want {
				t.Fatalf("needsADBUnlockForVoice() = %t; want %t", got, test.want)
			}
		})
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

// The fake transport exercises configuration/reboot state transitions without
// writing to a connected modem. Only known non-secret setup commands are used.
func setupRecoveryFixture(t *testing.T) (*app, moduleSetupBackup) {
	t.Helper()
	a := &app{demo: true, deviceID: "fixture", moduleSetupBackupDir: t.TempDir(), moduleSetupPath: filepath.Join(t.TempDir(), "state.json")}
	a.usbLocator = &usbDeviceLocator{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Bus: 2, Address: 7, PortPath: []uint8{1, 1, 1}}
	backup := moduleSetupBackup{FormatVersion: 1, DeviceID: a.deviceID, USB: usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}}
	return a, backup
}

func setupReadback(usb usbComposition, ims, capability int) func(string, time.Duration) (string, error) {
	return func(command string, _ time.Duration) (string, error) {
		switch command {
		case `AT+QCFG="USBCFG"`:
			return strings.Replace(usb.command(), `AT+QCFG=`, `+QCFG: `, 1) + "\r\nOK", nil
		case `AT+QCFG="ims"`:
			return fmt.Sprintf("+QCFG: \"ims\",%d,%d\r\nOK", ims, capability), nil
		case `AT+QCFG="volte_disable"`:
			return "+QCFG: \"volte/disable\",0\r\nOK", nil
		default:
			return "OK", nil
		}
	}
}

func TestModuleSetupRollbackWaitsForReenumerationAndPreservesCause(t *testing.T) {
	for _, usbError := range []string{"LIBUSB_ERROR_IO", "LIBUSB_ERROR_PIPE", "LIBUSB_ERROR_NO_DEVICE", "USB AT command timed out without response"} {
		t.Run(usbError, func(t *testing.T) {
			a, backup := setupRecoveryFixture(t)
			path, err := a.saveModuleSetupBackup("tx", backup)
			if err != nil {
				t.Fatal(err)
			}
			readback := setupReadback(backup.USB, 0, 0)
			reboots := 0
			a.moduleSetupAT = func(command string, timeout time.Duration) (string, error) {
				if command == "AT+CFUN=1,1" {
					reboots++
					return "", errors.New(usbError)
				}
				return readback(command, timeout)
			}
			a.setModuleSetup(moduleSetupStatus{TransactionID: "tx", State: "initializing", Stage: "configure"})
			cause := "USB 配置回读不一致：ADB 位仍为 0"
			a.rollbackModuleSetup("tx", path, backup, cause)
			pending := a.moduleSetup
			if pending.State != "rollback_restarting" || pending.OriginalError != cause || pending.RestartError != usbError {
				t.Fatalf("pending rollback lost diagnostics: %#v", pending)
			}
			// Even a successful AT read on the old enumeration must not count.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			err = a.waitModuleSetupVerification(ctx, pending)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) || a.moduleSetup.State == "rolled_back" {
				t.Fatalf("old enumeration accepted: %v, %#v", err, a.moduleSetup)
			}
			a.usbLocator.Address++
			a.resumeModuleSetup(context.Background())
			if a.moduleSetup.State != "rolled_back" || a.moduleSetup.OriginalError != cause || reboots != 1 {
				t.Fatalf("rollback verification/reboot replay: %#v, reboots=%d", a.moduleSetup, reboots)
			}
			reloaded := &app{moduleSetupPath: a.moduleSetupPath}
			if err := reloaded.loadModuleSetupState(); err != nil {
				t.Fatal(err)
			}
			if reloaded.moduleSetup.OriginalError != cause || reloaded.moduleSetup.RestartError != usbError {
				t.Fatalf("persisted diagnostics lost: %#v", reloaded.moduleSetup)
			}
		})
	}
}

func TestModuleSetupRollbackErrorsDoNotOverwriteOriginal(t *testing.T) {
	a, backup := setupRecoveryFixture(t)
	a.setModuleSetup(moduleSetupStatus{TransactionID: "tx", State: "initializing", Stage: "configure"})
	a.moduleSetupAT = func(string, time.Duration) (string, error) { return "", errors.New("LIBUSB_ERROR_PIPE") }
	a.rollbackModuleSetup("tx", "backup.json", backup, "IMS 回读不一致")
	if a.moduleSetup.State != "failed" || a.moduleSetup.OriginalError != "IMS 回读不一致" || !strings.Contains(a.moduleSetup.Detail, "LIBUSB_ERROR_PIPE") || a.moduleSetup.Stage != "rollback" {
		t.Fatalf("original failure overwritten: %#v", a.moduleSetup)
	}
}

func TestModuleSetupRebootRejectionAndIncompleteReplies(t *testing.T) {
	for _, response := range []string{"ERROR", "+CME ERROR: 3", "AT+CFUN=1,1", ""} {
		t.Run(response, func(t *testing.T) {
			a, _ := setupRecoveryFixture(t)
			a.moduleSetupAT = func(string, time.Duration) (string, error) { return response, nil }
			if err := a.requestModuleSetupRestart("tx", "backup.json", false); err == nil {
				t.Fatalf("unconfirmed reboot accepted: %q", response)
			}
			if err := a.writeSetupCommand(`AT+QCFG="ims",1`); err == nil {
				t.Fatalf("unconfirmed config write accepted: %q", response)
			}
		})
	}
}

func TestModuleSetupIMSRuntimeCapabilityCheckedAfterReboot(t *testing.T) {
	a, backup := setupRecoveryFixture(t)
	target := targetVoiceUSB(backup.USB)
	a.moduleSetupAT = setupReadback(target, 1, 0)
	if err := a.writeAndVerifySetup(target, 1, 0); err != nil {
		t.Fatalf("stored IMS setting must be allowed before restart: %v", err)
	}
	if err := a.verifyCurrentVoiceSetup(); err == nil {
		t.Fatal("runtime capability must be ready after restart")
	}
	a.moduleSetupAT = setupReadback(target, 1, 1)
	if err := a.verifyCurrentVoiceSetup(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleSetupRollbackVerificationTimeoutNeverReportsSuccess(t *testing.T) {
	a, backup := setupRecoveryFixture(t)
	path, err := a.saveModuleSetupBackup("tx", backup)
	if err != nil {
		t.Fatal(err)
	}
	a.setModuleSetup(moduleSetupStatus{TransactionID: "tx", State: "rollback_restarting", Rollback: true, Stage: "rollback_restart", BackupPath: path, OriginalError: "USB 回读不一致", RestartError: "LIBUSB_ERROR_IO", VerifyDeadline: time.Now().Add(-time.Second)})
	a.moduleSetupAT = func(string, time.Duration) (string, error) {
		t.Fatal("expired transaction replayed an AT command")
		return "", nil
	}
	a.resumeModuleSetup(context.Background())
	if a.moduleSetup.State != "failed" || a.moduleSetup.OriginalError != "USB 回读不一致" || !strings.Contains(a.moduleSetup.Detail, "超时") {
		t.Fatalf("false recovery success: %#v", a.moduleSetup)
	}
}

func TestModuleSetupVerificationRejectsDifferentUSBPort(t *testing.T) {
	a, backup := setupRecoveryFixture(t)
	path, err := a.saveModuleSetupBackup("tx", backup)
	if err != nil {
		t.Fatal(err)
	}
	pending := moduleSetupStatus{TransactionID: "tx", State: "rollback_verifying", Rollback: true, BackupPath: path, RestartEnumeration: setupEnumeration(a.usbLocator), RestartPhysicalID: a.usbLocator.PhysicalID(), VerifyDeadline: time.Now().Add(time.Second)}
	a.setModuleSetup(pending)
	a.usbLocator.Address++
	a.usbLocator.PortPath = []uint8{1, 1, 2}
	a.moduleSetupAT = func(string, time.Duration) (string, error) {
		t.Fatal("verification touched another USB port")
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := a.waitModuleSetupVerification(ctx, pending); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestModuleSetupInitializationSummaryReflectsUAC(t *testing.T) {
	if got := setupInitializationSummary(false, true); strings.Contains(got, "已启用") {
		t.Fatal(got)
	}
	if got := setupInitializationSummary(true, true); !strings.Contains(got, "USB 音频已启用") {
		t.Fatal(got)
	}
}

func TestModuleSetupStartIsExclusiveAndClearsOldDiagnostics(t *testing.T) {
	a, _ := setupRecoveryFixture(t)
	a.setModuleSetup(moduleSetupStatus{TransactionID: "old", State: "failed", OriginalError: "old failure"})
	results := make(chan bool, 2)
	for _, id := range []string{"first", "second"} {
		go func() { results <- a.beginModuleSetup(moduleSetupStatus{TransactionID: id, State: "initializing"}) }()
	}
	first, second := <-results, <-results
	if first == second {
		t.Fatalf("expected exactly one setup to start: %v, %v", first, second)
	}
	if a.moduleSetup.OriginalError != "" {
		t.Fatal("new transaction retained an old failure")
	}
	currentID := a.moduleSetup.TransactionID
	a.updateModuleSetup("old", moduleSetupStatus{State: "failed", OriginalError: "late failure"})
	if a.moduleSetup.TransactionID != currentID || a.moduleSetup.State != "initializing" {
		t.Fatal("old worker overwrote a newer transaction")
	}
}

func TestModuleSetupRejectsPartialReadback(t *testing.T) {
	a, backup := setupRecoveryFixture(t)
	a.moduleSetupAT = func(string, time.Duration) (string, error) {
		return strings.Replace(backup.USB.command(), `AT+QCFG=`, `+QCFG: `, 1), nil
	}
	if err := a.verifyModuleSetupValues(backup.USB, 0, 0, false); err == nil || !strings.Contains(err.Error(), "完整 OK") {
		t.Fatalf("partial response accepted: %v", err)
	}
}

func TestModuleSetupVerificationRetriesTransientReadFailure(t *testing.T) {
	a, backup := setupRecoveryFixture(t)
	path, err := a.saveModuleSetupBackup("tx", backup)
	if err != nil {
		t.Fatal(err)
	}
	status := moduleSetupStatus{TransactionID: "tx", State: "rollback_verifying", Rollback: true, OriginalError: "original failure", BackupPath: path, VerifyDeadline: time.Now().Add(5 * time.Second)}
	a.setModuleSetup(status)
	reads := 0
	readback := setupReadback(backup.USB, 0, 0)
	a.moduleSetupAT = func(command string, timeout time.Duration) (string, error) {
		if !strings.HasPrefix(command, `AT+QCFG="`) || (!strings.HasSuffix(command, `"`)) {
			t.Fatalf("verification replayed a write: %s", command)
		}
		reads++
		if reads == 1 {
			return "", errors.New("LIBUSB_ERROR_NO_DEVICE")
		}
		return readback(command, timeout)
	}
	a.resumeModuleSetup(context.Background())
	if a.moduleSetup.State != "rolled_back" || reads != 4 || a.moduleSetup.OriginalError != "original failure" {
		t.Fatalf("transient verification failed: %#v, reads=%d", a.moduleSetup, reads)
	}
}
