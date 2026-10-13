package relayclient

import (
	"context"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"net/http"
)

func (c *Client) ManagedInstallations(ctx context.Context, access string) ([]identity.ManagedIdentity, error) {
	var result struct {
		Installations []identity.ManagedIdentity `json:"installations"`
	}
	err := c.requestLimit(ctx, http.MethodGet, "/v1/management/installations", access, nil, &result, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(result.Installations) > 1024 {
		return nil, ErrRequest
	}
	return result.Installations, nil
}
func (c *Client) PairingStatus(ctx context.Context, access, id string) (identity.GrantStatus, error) {
	var result identity.GrantStatus
	if !callsignal.ID(id) {
		return result, ErrRequest
	}
	err := c.request(ctx, http.MethodGet, "/v1/management/pairings/"+id, access, nil, &result)
	if err == nil && result.ID != id {
		return result, ErrRequest
	}
	return result, err
}
func (c *Client) ManagedPairings(ctx context.Context, access string) ([]identity.GrantStatus, error) {
	var result struct {
		Pairings []identity.GrantStatus `json:"pairings"`
	}
	if err := c.requestLimit(ctx, http.MethodGet, "/v1/management/pairings", access, nil, &result, 1<<20); err != nil {
		return nil, err
	}
	if len(result.Pairings) > 128 {
		return nil, ErrRequest
	}
	return result.Pairings, nil
}
func (c *Client) RevokePairing(ctx context.Context, access, id string) error {
	if !callsignal.ID(id) {
		return ErrRequest
	}
	var result struct {
		Revoked bool `json:"revoked"`
	}
	if err := c.request(ctx, http.MethodDelete, "/v1/management/pairings/"+id, access, nil, &result); err != nil {
		return err
	}
	if !result.Revoked {
		return ErrRequest
	}
	return nil
}
func (c *Client) NameManaged(ctx context.Context, access, kind, id, name string) error {
	if !callsignal.ID(id) || (kind != "installations" && kind != "pairings") {
		return ErrRequest
	}
	var result struct {
		Updated bool `json:"updated"`
	}
	if err := c.request(ctx, http.MethodPatch, "/v1/management/"+kind+"/"+id, access, map[string]string{"name": name}, &result); err != nil {
		return err
	}
	if !result.Updated {
		return ErrRequest
	}
	return nil
}
