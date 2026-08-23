package main

import (
	"bytes"
	"testing"
)

func TestADBCommandUsesWireByteOrder(t *testing.T) {
	if got, want := adbCommand("CNXN"), uint32(0x4e584e43); got != want {
		t.Fatalf("adbCommand(CNXN) = 0x%08x, want 0x%08x", got, want)
	}
}

func TestADBHeaderRoundTrip(t *testing.T) {
	payload := []byte("host::DJOneHub\x00")
	header := adbEncodeHeader(adbCommand("CNXN"), adbVersion, adbMaxPayload, payload)
	message, checksum, err := adbDecodeHeader(header)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if message.command != adbCommand("CNXN") || message.arg0 != adbVersion || message.arg1 != adbMaxPayload {
		t.Fatalf("decoded header = %#v", message)
	}
	if checksum != adbChecksum(payload) {
		t.Fatalf("checksum = %d, want %d", checksum, adbChecksum(payload))
	}
	if bytes.Equal(header[20:24], header[0:4]) {
		t.Fatal("ADB magic must be the command complement")
	}
}

func TestADBDecodeHeaderRejectsInvalidMagicAndOversizedPayload(t *testing.T) {
	header := adbEncodeHeader(adbCommand("WRTE"), 1, 2, nil)
	header[20] ^= 1
	if _, _, err := adbDecodeHeader(header); err == nil {
		t.Fatal("invalid magic was accepted")
	}
	header = adbEncodeHeader(adbCommand("WRTE"), 1, 2, nil)
	lePutUint32(header[12:], adbMaxPayload+1)
	if _, _, err := adbDecodeHeader(header); err == nil {
		t.Fatal("oversized payload was accepted")
	}
}

func TestParseADBStatusUsesLastCompleteMarker(t *testing.T) {
	raw := "old __DJONEHUB_STATUS_token_1__\nnew\n__DJONEHUB_STATUS_token_17__\n"
	if got, ok := parseADBStatus(raw, "token"); !ok || got != 17 {
		t.Fatalf("parseADBStatus = %d, %v; want 17, true", got, ok)
	}
	if _, ok := parseADBStatus("plain output", "token"); ok {
		t.Fatal("missing status marker was accepted")
	}
}

func TestADBTokenIsRandomHex(t *testing.T) {
	first := adbToken()
	second := adbToken()
	if len(first) != 32 || len(second) != 32 || first == second {
		t.Fatalf("unexpected ADB tokens %q and %q", first, second)
	}
}
