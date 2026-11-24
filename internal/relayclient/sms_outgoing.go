package relayclient

import (
	"context"
	"github.com/iniwex5/vohive/internal/smsout"
	"net/http"
)

func (c *Client) ClaimOutgoingSMS(ctx context.Context, access string) (*smsout.Request, error) {
	var response struct {
		Request *smsout.Request `json:"request"`
	}
	err := c.request(ctx, http.MethodPost, "/v1/sms/outgoing/claim", access, struct{}{}, &response)
	if err != nil {
		return nil, err
	}
	if response.Request != nil && response.Request.Validate() != "" {
		return nil, ErrRequest
	}
	return response.Request, nil
}

func (c *Client) FinishOutgoingSMS(ctx context.Context, access string, result smsout.Result) error {
	var response struct {
		Updated bool `json:"updated"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/sms/outgoing/result", access, result, &response); err != nil {
		return err
	}
	if !response.Updated {
		return ErrRequest
	}
	return nil
}
