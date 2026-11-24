package relayclient

import (
	"context"
	"net/http"

	"github.com/iniwex5/vohive/internal/voippush"
)

func (c *Client) SendSMS(ctx context.Context, access string, sms voippush.SMS) error {
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if !sms.Valid() {
		return ErrRequest
	}
	if err := c.request(ctx, http.MethodPost, "/v1/sms-notifications", access, sms, &result); err != nil {
		return err
	}
	if !result.Accepted {
		return ErrRequest
	}
	return nil
}
