package main

import "testing"

func TestPlaintextRequiresExplicitLiteralLoopback(t *testing.T) {
	for _, tc := range []struct {
		listen            string
		insecure, allowed bool
	}{{"127.0.0.1:8443", false, false}, {"127.0.0.1:8443", true, true}, {"[::1]:8443", true, true}, {"0.0.0.0:8443", true, false}, {"localhost:8443", true, false}, {":8443", true, false}} {
		err := validateTransport(tc.listen, "", "", tc.insecure)
		if (err == nil) != tc.allowed {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if err := validateTransport(":8443", "cert.pem", "key.pem", false); err != nil {
		t.Fatal(err)
	}
	if err := validateTransport(":8443", "cert.pem", "", false); err == nil {
		t.Fatal("partial TLS config accepted")
	}
}
