package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestParseUSBCFG(t *testing.T) {
	parts, ok := parseUSBCFG("AT+QCFG\r\n+qcfg: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK")
	if !ok {
		t.Fatal("parseUSBCFG() failed")
	}
	if got, want := parts[len(parts)-1], "0"; got != want {
		t.Fatalf("last part=%q want=%q parts=%v", got, want, parts)
	}
	command, ok := enableUACInUSBCFG(parts)
	if !ok || command != `AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1` {
		t.Fatalf("command=%q ok=%v", command, ok)
	}
}

func TestUSBCFGWithSixFunctionParametersDoesNotExposeUAC(t *testing.T) {
	parts, ok := parseUSBCFG(`+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0` + "\r\nOK")
	if !ok {
		t.Fatal("parseUSBCFG() failed")
	}
	if got := usbFunctionCount(parts); got != 6 {
		t.Fatalf("usbFunctionCount()=%d want=6", got)
	}
	if _, known := usbConfigUACEnabled(parts); known {
		t.Fatal("six-function USBCFG unexpectedly exposed a UAC flag")
	}
	if command, ok := enableUACInUSBCFG(parts); ok || command != "" {
		t.Fatalf("command=%q ok=%v", command, ok)
	}
}

func TestParseQPCMVStatus(t *testing.T) {
	enabled, mode, ok := parseQPCMVStatus("+QPCMV: 1,2\r\nOK")
	if !ok || !enabled || mode != 2 {
		t.Fatalf("enabled=%v mode=%d ok=%v", enabled, mode, ok)
	}
	if _, _, ok := parseQPCMVStatus("ERROR"); ok {
		t.Fatal("ERROR unexpectedly parsed as QPCMV status")
	}
	enabled, mode, ok = parseQPCMVStatus("+QPCMV: 0\r\nOK")
	if !ok || enabled || mode != 0 {
		t.Fatalf("optional mode parse: enabled=%v mode=%d ok=%v", enabled, mode, ok)
	}
}

func TestCallAudioStatus(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,1\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
		callAudioRuntimeQuery:   "+QPCMV: 1,2\r\nOK",
	})
	a := &app{callATRunner: runner.run}
	capability, err := a.callAudioStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !capability.Supported || !capability.USBConfigKnown || !capability.UACConfigured ||
		!capability.RuntimeControlAvailable || !capability.RuntimeStatusKnown ||
		!capability.Enabled || capability.Mode != 2 {
		t.Fatalf("capability=%+v", capability)
	}
}

func TestCallAudioStatusDoesNotTreatAdvertisedQPCMVAsRuntimeSupport(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2CA3,0x4006,1,1,1,1,1,0,1\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0-2)\r\nOK",
		callAudioRuntimeQuery:   "ERROR",
	})
	a := &app{callATRunner: runner.run}
	capability, err := a.callAudioStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !capability.Supported || capability.RuntimeControlAvailable || capability.RuntimeStatusKnown ||
		len(capability.Diagnostics) == 0 {
		t.Fatalf("capability=%+v", capability)
	}
}

func TestCallAudioStatusReportsUnsupportedFirmware(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "ERROR",
	})
	a := &app{callATRunner: runner.run}
	capability, err := a.callAudioStatus()
	if err != nil {
		t.Fatal(err)
	}
	if capability.Supported || !capability.NeedsUSBReconfigure || len(capability.Diagnostics) == 0 {
		t.Fatalf("capability=%+v", capability)
	}
}

func TestEnableCallAudioRequiresExplicitUSBConfirmation(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
	})
	a := &app{callATRunner: runner.run}
	_, err := a.enableCallAudio(false)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "uac_reconfigure_confirmation_required" {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(runner.commands, []string{callAudioUSBConfigQuery, callAudioSupportQuery}) {
		t.Fatalf("commands=%v", runner.commands)
	}
}

func TestEnableCallAudioUpdatesUSBConfigWithoutRebooting(t *testing.T) {
	const updateCommand = `AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1`
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
		updateCommand:           "OK",
	})
	a := &app{callATRunner: runner.run}
	result, err := a.enableCallAudio(true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RestartRequired || result.Status != "restart_required" {
		t.Fatalf("result=%+v", result)
	}
	if !reflect.DeepEqual(runner.commands, []string{callAudioUSBConfigQuery, callAudioSupportQuery, updateCommand}) {
		t.Fatalf("commands=%v", runner.commands)
	}
}

func TestEnableCallAudioDoesNotModifyUnsupportedFirmware(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "ERROR",
	})
	a := &app{callATRunner: runner.run}
	_, err := a.enableCallAudio(true)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "call_audio_unsupported" {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(runner.commands, []string{callAudioUSBConfigQuery, callAudioSupportQuery}) {
		t.Fatalf("commands=%v", runner.commands)
	}
}

func TestEnableCallAudioRejectsSixFunctionUSBConfigWithoutWriting(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0\r\nOK",
	})
	a := &app{callATRunner: runner.run}
	_, err := a.enableCallAudio(true)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "uac_usb_config_unsupported" {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(runner.commands, []string{callAudioUSBConfigQuery}) {
		t.Fatalf("commands=%v", runner.commands)
	}
}

func TestEnableCallAudioReportsUSBConfigWriteRejection(t *testing.T) {
	const updateCommand = `AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1`
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
		updateCommand:           "ERROR",
	})
	a := &app{callATRunner: runner.run}
	_, err := a.enableCallAudio(true)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "uac_usb_config_rejected" || controlErr.Detail == "" {
		t.Fatalf("err=%#v", err)
	}
}

func TestEnableCallAudioRuntimeMode(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,1\r\nOK",
		callAudioEnableCommand:  "OK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
	})
	runner.responseSequences = map[string][]string{
		callAudioRuntimeQuery: {"+QPCMV: 0,2\r\nOK", "+QPCMV: 1,2\r\nOK"},
	}
	a := &app{callATRunner: runner.run}
	result, err := a.enableCallAudio(false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Status != "enabled" || result.Capability == nil || !result.Capability.Enabled {
		t.Fatalf("result=%+v", result)
	}
}

func TestEnableCallAudioReportsRuntimeRejection(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,1\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
		callAudioRuntimeQuery:   "+QPCMV: 0,2\r\nOK",
		callAudioEnableCommand:  "+CME ERROR: 3",
	})
	a := &app{callATRunner: runner.run}
	_, err := a.enableCallAudio(false)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "uac_runtime_enable_rejected" || controlErr.Detail == "" {
		t.Fatalf("err=%#v", err)
	}
}

func TestEnableCallAudioStopsWhenRuntimeReadIsRejected(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2CA3,0x4006,1,1,1,1,1,0,1\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0-2)\r\nOK",
		callAudioRuntimeQuery:   "ERROR",
	})
	a := &app{callATRunner: runner.run}
	_, err := a.enableCallAudio(false)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "uac_runtime_control_unavailable" || controlErr.Detail == "" {
		t.Fatalf("err=%#v", err)
	}
	if !reflect.DeepEqual(runner.commands,
		[]string{callAudioUSBConfigQuery, callAudioSupportQuery, callAudioRuntimeQuery}) {
		t.Fatalf("commands=%v", runner.commands)
	}
}

func TestEnableCallAudioBlockedDuringCall(t *testing.T) {
	a := &app{activeCall: &callRecord{State: "active"}}
	_, err := a.enableCallAudio(true)
	controlErr, ok := err.(*callControlError)
	if !ok || controlErr.Code != "call_state_conflict" {
		t.Fatalf("err=%v", err)
	}
}

func TestCallAudioHTTP(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,1\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
		callAudioRuntimeQuery:   "+QPCMV: 1,2\r\nOK",
	})
	a := &app{callATRunner: runner.run}
	response := performCallRequest(t, a, http.MethodGet, "/api/calls/audio", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEnableCallAudioHTTPRequiresExplicitConfirmation(t *testing.T) {
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
	})
	a := &app{callATRunner: runner.run}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/audio/enable", `{}`)
	assertCallError(t, response, http.StatusConflict, "uac_reconfigure_confirmation_required")
}

func TestEnableCallAudioHTTPReturnsStageDiagnostic(t *testing.T) {
	const updateCommand = `AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1`
	runner := newCallAudioTestRunner(map[string]string{
		callAudioUSBConfigQuery: "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
		callAudioSupportQuery:   "+QPCMV: (0,1),(0,1,2)\r\nOK",
		updateCommand:           "ERROR",
	})
	a := &app{callATRunner: runner.run}
	response := performCallRequest(t, a, http.MethodPost, "/api/calls/audio/enable",
		`{"allow_usb_reconfigure":true}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "uac_usb_config_rejected" || body["detail"] == "" {
		t.Fatalf("body=%v", body)
	}
}

type callAudioTestRunner struct {
	responses         map[string]string
	responseSequences map[string][]string
	commands          []string
}

func newCallAudioTestRunner(responses map[string]string) *callAudioTestRunner {
	return &callAudioTestRunner{responses: responses}
}

func (r *callAudioTestRunner) run(command string, _ time.Duration) (string, error) {
	r.commands = append(r.commands, command)
	if sequence := r.responseSequences[command]; len(sequence) > 0 {
		response := sequence[0]
		r.responseSequences[command] = sequence[1:]
		return response, nil
	}
	if response, ok := r.responses[command]; ok {
		return response, nil
	}
	return "ERROR", nil
}
