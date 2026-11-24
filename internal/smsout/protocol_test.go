package smsout

import (
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestSMSValidationAndMultipartLimits(t *testing.T) {
	r := Request{ID: uuid.NewString(), ModuleID: "module", Phone: "+8613800138000", Message: "测试🙂", CreatedAt: time.Now()}
	if code := r.Validate(); code != "" {
		t.Fatal(code)
	}
	for _, number := range []string{"", "+", "10086;ATD123", "+86\r10086", "１２３４", "123*45", strings.Repeat("1", 21)} {
		r.Phone = number
		if r.Validate() != "invalid_number" {
			t.Fatal(number)
		}
	}
	r.Phone = "10086"
	for _, message := range []string{"", " \n ", "a\x00b"} {
		r.Message = message
		if r.Validate() != "invalid_message" {
			t.Fatal("bad body accepted")
		}
	}
	r.Message = strings.Repeat("中", 670)
	if r.Validate() != "" {
		t.Fatal("10 segments rejected")
	}
	r.Message += "中"
	if r.Validate() != "message_too_long" {
		t.Fatal("oversize accepted")
	}
}

func TestNormalizeMainlandSMSPhone(t *testing.T) {
	for input, want := range map[string]string{
		"13800138000": "+8613800138000", "19912345678": "+8619912345678",
		"8613800138000": "+8613800138000", "008613800138000": "+8613800138000",
		"+8613800138000": "+8613800138000", "+14155552671": "+14155552671",
		"10086": "10086", "95588": "95588", "02112345678": "02112345678",
		"12345678901": "12345678901", "1380013800": "1380013800",
		"138001380000": "138001380000", "": "", "1380013800x": "1380013800x",
	} {
		if got := NormalizePhone(input); got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", input, got, want)
		}
		if got := NormalizePhone(want); got != want {
			t.Errorf("normalization is not idempotent for %q", want)
		}
	}
}
