// Package callsignal defines the bounded, versioned remote call signaling wire.
// It transports control and SDP/ICE only, never PCM or arbitrary Hub requests.
package callsignal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const Subprotocol = "djonehub.calls.v1"
const MaxMessage = 128 * 1024

var ErrProtocol = errors.New("invalid call signaling message")

type Message struct {
	Version        int             `json:"version"`
	Type           string          `json:"type"`
	HubEpoch       string          `json:"hub_epoch,omitempty"`
	ModuleID       string          `json:"module_id,omitempty"`
	CallUUID       string          `json:"call_uuid,omitempty"`
	Generation     uint64          `json:"generation,omitempty"`
	InstallationID string          `json:"installation_id,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

const RecoveryHeader = "X-DJOneHub-Recovery"
const ResumeHeader = "X-DJOneHub-Resume"

type Welcome struct {
	Recovery        bool      `json:"recovery,omitempty"`
	Resumed         bool      `json:"resumed,omitempty"`
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	HubID           string    `json:"hub_id"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
}
type Reauthorize struct {
	AccessToken string `json:"access_token"`
}
type Authorization struct {
	AccessExpiresAt time.Time `json:"access_expires_at"`
}
type DialRequest struct {
	Number string `json:"number"`
}

// Voice dialing accepts only digits and one leading +; never AT syntax or USSD.
func DialNumber(number string) bool {
	if len(number) == 0 || len(number) > 32 {
		return false
	}
	digits := number
	if digits[0] == '+' {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type Invite struct {
	Deadline  time.Time `json:"deadline"`
	Display   string    `json:"display"`
	Direction string    `json:"direction,omitempty"`
}
type SDP struct {
	CallControls   bool   `json:"call_controls,omitempty"`
	AudioRecovery  bool   `json:"audio_recovery,omitempty"`
	SignalRecovery bool   `json:"signal_recovery,omitempty"`
	Type           string `json:"type"`
	SDP            string `json:"sdp"`
	Recovery       bool   `json:"recovery,omitempty"`
}
type RestartSDP struct {
	ID  uint64 `json:"negotiation_id"`
	SDP string `json:"sdp"`
}
type ICE struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}
type Command struct {
	Digit            string `json:"digit,omitempty"`
	Action           string `json:"action"`
	ExpectedRevision uint64 `json:"expected_revision"`
}
type Mute struct {
	Sequence uint64 `json:"sequence"`
	Muted    bool   `json:"muted"`
}

func DTMFDigit(digit string) bool {
	return len(digit) == 1 && ((digit[0] >= '0' && digit[0] <= '9') || digit == "*" || digit == "#")
}

type Audio struct {
	Sequence uint64 `json:"sequence"`
	Active   bool   `json:"active"`
}
type State struct {
	Revision uint64 `json:"revision"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
}
type Result struct {
	Accepted bool   `json:"accepted"`
	Code     string `json:"code,omitempty"`
}
type Failure struct {
	Code string `json:"code"`
}

func Body(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
func Decode(data []byte, into any) error {
	trimmed := bytes.TrimSpace(data)
	if len(data) > MaxMessage || !utf8.Valid(data) || len(trimmed) < 2 || trimmed[0] != '{' {
		return ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return ErrProtocol
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrProtocol
	}
	return nil
}
func ID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Validate enforces direction and payload shape. Authentication, invitation
// ownership and deadlines must additionally be checked by the relay/Hub.
func (m Message) Validate(kind string) error {
	if kind != "hub" && kind != "installation" {
		return ErrProtocol
	}
	if m.Type == "reauth" {
		var p Reauthorize
		if m.connectionMessage() && Decode(m.Payload, &p) == nil && len(p.AccessToken) == 43 && ID(p.AccessToken) {
			return nil
		}
		return ErrProtocol
	}
	if m.validateBinding() != nil {
		return ErrProtocol
	}
	return m.validatePayload(kind)
}
func (m Message) connectionMessage() bool {
	return m.Version == 1 && ID(m.RequestID) && m.HubEpoch == "" && m.ModuleID == "" && m.CallUUID == "" && m.Generation == 0 && m.InstallationID == ""
}
func (m Message) validateBinding() error {
	parsed, err := uuid.Parse(m.CallUUID)
	if m.Version != 1 || err != nil || parsed.String() != m.CallUUID || !ID(m.HubEpoch) || !ID(m.ModuleID) || !ID(m.InstallationID) || m.Generation == 0 || !ID(m.RequestID) {
		return ErrProtocol
	}
	return nil
}
func (m Message) validatePayload(kind string) error {
	valid := false
	switch m.Type {
	case "dial":
		var p DialRequest
		valid = kind == "installation" && m.Generation == 1 && Decode(m.Payload, &p) == nil && DialNumber(p.Number)
	case "cancel_dial":
		var p struct{}
		valid = kind == "installation" && m.Generation == 1 && Decode(m.Payload, &p) == nil
	case "invite":
		var p Invite
		valid = kind == "hub" && Decode(m.Payload, &p) == nil && !p.Deadline.IsZero() && len(p.Display) <= 128 && (p.Direction == "" || p.Direction == "incoming" || p.Direction == "outgoing")
	case "offer", "answer_sdp":
		var p SDP
		want, sender := "offer", "hub"
		if m.Type == "answer_sdp" {
			want, sender = "answer", "installation"
		}
		valid = kind == sender && Decode(m.Payload, &p) == nil && p.Type == want && len(p.SDP) > 0 && len(p.SDP) <= 65536 && (m.Type == "offer" || (!p.Recovery && !p.SignalRecovery && !p.AudioRecovery && !p.CallControls))
	case "restart_offer", "restart_answer":
		var p RestartSDP
		sender := "hub"
		if m.Type == "restart_answer" {
			sender = "installation"
		}
		valid = kind == sender && Decode(m.Payload, &p) == nil && p.ID > 0 && len(p.SDP) > 0 && len(p.SDP) <= 65536
	case "ice":
		var p ICE
		valid = Decode(m.Payload, &p) == nil && len(p.Candidate) <= 4096 && (p.SDPMid == nil || len(*p.SDPMid) <= 128) && (p.UsernameFragment == nil || len(*p.UsernameFragment) <= 256)
	case "sync":
		var p struct{}
		valid = kind == "installation" && Decode(m.Payload, &p) == nil
	case "command":
		var p Command
		valid = kind == "installation" && Decode(m.Payload, &p) == nil && p.ExpectedRevision > 0 && ((p.Action == "dtmf" && DTMFDigit(p.Digit)) || (p.Digit == "" && (p.Action == "answer" || p.Action == "reject" || p.Action == "hangup")))
	case "mute":
		var p Mute
		valid = kind == "installation" && Decode(m.Payload, &p) == nil && p.Sequence > 0
	case "audio_active":
		var p Audio
		valid = kind == "installation" && Decode(m.Payload, &p) == nil && p.Sequence > 0
	case "state":
		var p State
		valid = kind == "hub" && Decode(m.Payload, &p) == nil && p.Revision > 0 && len(p.Reason) <= 128 && (p.State == "offered" || p.State == "answering" || p.State == "rejecting" || p.State == "awaiting_audio" || p.State == "connected" || p.State == "ended")
	case "result":
		var p Result
		valid = kind == "hub" && Decode(m.Payload, &p) == nil && len(p.Code) <= 128
	}
	if !valid {
		return ErrProtocol
	}
	return nil
}

// ValidateDelivery additionally allows the relay's delivery/failure events.
func (m Message) ValidateDelivery(receiver string) error {
	if m.Type == "reauthenticated" {
		var p Authorization
		if m.connectionMessage() && Decode(m.Payload, &p) == nil && !p.AccessExpiresAt.IsZero() {
			return nil
		}
		return ErrProtocol
	}
	if m.validateBinding() != nil {
		return ErrProtocol
	}
	switch m.Type {
	case "error", "peer_closed", "peer_reconnecting", "peer_resumed":
		var p Failure
		if Decode(m.Payload, &p) == nil && ID(p.Code) {
			return nil
		}
	case "dial_accepted":
		var p Result
		if receiver == "installation" && Decode(m.Payload, &p) == nil && p.Accepted {
			return nil
		}
	case "delivered", "waking":
		var p Result
		if receiver == "hub" && Decode(m.Payload, &p) == nil && p.Accepted {
			return nil
		}
	default:
		sender := "hub"
		if receiver == "hub" {
			sender = "installation"
		}
		return m.Validate(sender)
	}
	return ErrProtocol
}

func (m Message) SameCall(other Message) bool {
	return m.HubEpoch == other.HubEpoch && m.ModuleID == other.ModuleID && m.CallUUID == other.CallUUID && m.Generation == other.Generation && m.InstallationID == other.InstallationID
}

// DataChannel never authorizes a new call or a new hardware action. Only the
// pre-bound installation's state query/hangup can enter the same command path.
func (m Message) ValidateControl(sender string) error {
	if m.Validate(sender) != nil {
		return ErrProtocol
	}
	if sender == "installation" {
		if m.Type == "sync" {
			return nil
		}
		if m.Type == "command" {
			var command Command
			Decode(m.Payload, &command)
			if command.Action == "hangup" {
				return nil
			}
		}
	} else if sender == "hub" && (m.Type == "state" || m.Type == "result") {
		return nil
	}
	return ErrProtocol
}
