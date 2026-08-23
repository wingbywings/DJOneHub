//go:build darwin && cgo

package main

import "testing"

func TestVoiceCalibrationLogSuccessOverridesNonFatalACDBErrors(t *testing.T) {
	logText := "ACDBFILE_MGR: CmnDevinfo not found\nError: ACDB AFE returned = -19\n" +
		"ACDB -> Sent VocProc Cal! acdb_id 4 cap 2 enable 1"
	if !voiceCalibrationLogSucceeded(logText) {
		t.Fatal("VocProc completion marker should win over non-fatal AFE table warnings")
	}
	if voiceCalibrationLogSucceeded("Error: ACDB AFE returned = -19") {
		t.Fatal("errors without the VocProc completion marker must not be accepted")
	}
}
