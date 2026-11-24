// Package smsout defines the authenticated, idempotent phone-to-SIM SMS protocol.
package smsout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/pkg/smscodec"
)

const MaxSegments = 10

type Request struct {
	ID             string    `json:"id"`
	ModuleID       string    `json:"module_id"`
	Phone          string    `json:"phone"`
	Message        string    `json:"message"`
	CreatedAt      time.Time `json:"created_at"`
	InstallationID string    `json:"installation_id,omitempty"` // assigned by the authenticated Relay
}

type Result struct {
	ID             string `json:"id"`
	InstallationID string `json:"installation_id"`
	State          string `json:"state"`
	Segments       int    `json:"segments"`
	Code           string `json:"code"`
}

func PhoneValid(phone string) bool {
	digits := strings.TrimPrefix(phone, "+")
	if len(digits) < 1 || len(digits) > 20 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// NormalizePhone applies the mainland-mobile default only at submission time.
// Keep the original request unchanged in Relay so existing retry digests match.
func NormalizePhone(phone string) string {
	if !PhoneValid(phone) || strings.HasPrefix(phone, "+") {
		return phone
	}
	mobile := phone
	if len(phone) == 15 && strings.HasPrefix(phone, "0086") {
		mobile = phone[4:]
	} else if len(phone) == 13 && strings.HasPrefix(phone, "86") {
		mobile = phone[2:]
	}
	if len(mobile) == 11 && mobile[0] == '1' && mobile[1] >= '3' && mobile[1] <= '9' {
		return "+86" + mobile
	}
	return phone
}

func SubmitOptions(message string) smscodec.SubmitOptions {
	for _, r := range message {
		if r > 127 {
			return smscodec.SubmitOptions{Encoding: smscodec.SMSEncodingUCS2}
		}
	}
	return smscodec.SubmitOptions{}
}

func (r Request) Validate() string {
	if id, err := uuid.Parse(r.ID); err != nil || id.String() != r.ID || !callsignal.ID(r.ModuleID) || r.CreatedAt.IsZero() {
		return "invalid_request"
	}
	if !PhoneValid(r.Phone) {
		return "invalid_number"
	}
	if !utf8.ValidString(r.Message) || strings.TrimSpace(r.Message) == "" || strings.ContainsRune(r.Message, 0) {
		return "invalid_message"
	}
	if len(r.Message) > 8192 {
		return "message_too_long"
	}
	pdus, _, err := smscodec.BuildSubmitTPDUsWithOptions(r.Phone, r.Message, SubmitOptions(r.Message))
	if err != nil {
		return "invalid_message"
	}
	if len(pdus) > MaxSegments {
		return "message_too_long"
	}
	return ""
}

func (r Request) Digest() string {
	data, _ := json.Marshal(r)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r Result) Terminal() bool {
	return r.State == "sent" || r.State == "failed" || r.State == "unknown"
}

func (r Result) Valid() bool {
	if !r.Terminal() || r.Segments < 0 || r.Segments > MaxSegments {
		return false
	}
	if r.State == "sent" {
		return r.Code == "" && r.Segments > 0
	}
	if r.State == "unknown" {
		return r.Code == "submission_uncertain"
	}
	switch r.Code {
	case "module_unavailable", "sms_not_allowed", "module_busy", "invalid_request", "invalid_number", "invalid_message", "message_too_long", "request_expired", "unauthorized", "storage_unavailable":
		return r.Segments == 0
	}
	return false
}
