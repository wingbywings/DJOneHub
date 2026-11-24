package voippush

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/iniwex5/vohive/internal/callsignal"
)

// SMS uses a regular APNs token and the app's alert topic, never PushKit.
type SMS struct {
	ID             string    `json:"id"`
	HubID          string    `json:"hub_id"`
	ModuleID       string    `json:"module_id"`
	InstallationID string    `json:"installation_id"`
	Sender         string    `json:"sender"`
	Content        string    `json:"content"`
	Timestamp      time.Time `json:"timestamp"`
}

type SMSSender interface {
	Environment() string
	SendSMS(context.Context, string, SMS) (Result, error)
}

func (s SMS) Valid() bool {
	id, err := hex.DecodeString(s.ID)
	return err == nil && len(id) == 32 && hex.EncodeToString(id) == s.ID && callsignal.ID(s.ModuleID) && callsignal.ID(s.InstallationID) &&
		len(s.Sender) <= 256 && utf8.ValidString(s.Sender) && len(s.Content) <= 32768 && utf8.ValidString(s.Content) && !s.Timestamp.IsZero()
}

func (c *Client) SendSMS(ctx context.Context, token string, sms SMS) (Result, error) {
	if !ValidToken(token) || !sms.Valid() || !callsignal.ID(sms.HubID) {
		return Result{}, ErrDelivery
	}
	// Keep the content only in aps.alert.body, leaving room for long Unicode SMS.
	// APNs limits regular notifications to 4096 bytes, including JSON escaping.
	content := []rune(sms.Content)
	metadata := sms
	metadata.Content = ""
	for n := len(content); n >= 0; {
		body := string(content[:n])
		if n < len(content) {
			body += "…（完整短信请在 Hub 查看）"
		}
		payload, err := json.Marshal(map[string]any{
			"aps": map[string]any{
				"alert": map[string]string{"title": "SIM 短信 · " + sms.Sender, "body": body},
				"sound": "default", "thread-id": "sms-" + sms.ModuleID,
			},
			"sms": metadata,
		})
		if err != nil {
			return Result{}, ErrDelivery
		}
		if len(payload) <= 4096 {
			return c.sendPayload(ctx, token, payload, "alert", c.config.BundleID,
				strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10), "", sms.ID)
		}
		// Every removed rune frees at least one byte. Bound work for long messages.
		remove := (len(payload) - 4096 + 5) / 6
		if remove > n {
			remove = n
		}
		if remove == 0 {
			break
		}
		n -= remove
	}
	return Result{}, ErrDelivery
}
