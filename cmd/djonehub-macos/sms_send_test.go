package main

import (
	"strings"
	"testing"

	"github.com/iniwex5/vohive/pkg/smscodec"
)

func TestATResponseCompleteRecognizesModemErrors(t *testing.T) {
	for _, response := range []string{
		"\r\n+CMS ERROR: 500\r\n",
		"\r\n+CME ERROR: 30\r\n",
		"\r\nERROR\r\n",
	} {
		if !atResponseComplete(response) {
			t.Fatalf("atResponseComplete(%q) = false", response)
		}
	}
}

func TestATResponseHasPrompt(t *testing.T) {
	if !atResponseHasPrompt("AT+CMGS=23\r\n> ") {
		t.Fatal("SMS prompt was not detected")
	}
	if atResponseHasPrompt("AT+CMGS=23\r\nOK\r\n") {
		t.Fatal("normal AT response was mistaken for a prompt")
	}
}

func TestSMSSubmitOptionsUsesUCS2ForChinese(t *testing.T) {
	if got := smsSubmitOptions("验证码 1234").Encoding; got != smscodec.SMSEncodingUCS2 {
		t.Fatalf("Chinese encoding = %q, want %q", got, smscodec.SMSEncodingUCS2)
	}
	if got := smsSubmitOptions("hello 123").Encoding; got != "" {
		t.Fatalf("ASCII encoding = %q, want auto", got)
	}
}

func TestParseUSBATRegistration(t *testing.T) {
	status, ok := parseUSBATRegistration("AT+CREG?\r\n+CREG: 0,3\r\nOK", "CREG")
	if !ok || status != 3 {
		t.Fatalf("parseUSBATRegistration() = (%d, %t), want (3, true)", status, ok)
	}
	if _, ok := parseUSBATRegistration("+CEREG: 0,1\r\nOK", "CREG"); ok {
		t.Fatal("CEREG response was mistaken for CREG")
	}
}

func TestParseUSBATIMSConfiguration(t *testing.T) {
	configured, capable, ok := parseUSBATIMSConfiguration("+QCFG: \"ims\",0,1\r\nOK")
	if !ok || configured != 0 || !capable {
		t.Fatalf("parseUSBATIMSConfiguration() = (%d, %t, %t), want (0, true, true)", configured, capable, ok)
	}
	if _, _, ok := parseUSBATIMSConfiguration("ERROR"); ok {
		t.Fatal("ERROR response should not produce a known IMS configuration")
	}
}

func TestUSBATSMSTransportError(t *testing.T) {
	denied := "+CREG: 0,3\r\nOK"
	registered := "+CREG: 0,1\r\nOK"
	imsUnavailable := "+QCFG: \"ims\",0,0\r\nOK"
	imsAvailable := "+QCFG: \"ims\",0,1\r\nOK"

	err := usbATSMSTransportError(denied, imsUnavailable)
	if err == nil || !strings.Contains(err.Error(), "CREG=3") {
		t.Fatalf("denied circuit registration without IMS error = %v", err)
	}
	if err := usbATSMSTransportError(registered, imsUnavailable); err != nil {
		t.Fatalf("registered circuit transport was rejected: %v", err)
	}
	if err := usbATSMSTransportError(denied, imsAvailable); err != nil {
		t.Fatalf("available IMS transport was rejected: %v", err)
	}
	if err := usbATSMSTransportError(denied, "ERROR"); err != nil {
		t.Fatalf("unknown IMS state should not block a send attempt: %v", err)
	}
}
