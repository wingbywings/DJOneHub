package relayprofile

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

// InstallationObserver retains a bounded access credential after an explicit
// management read. Polling never reads the Vault or rotates a credential.
type InstallationObserver struct {
	client  *relayclient.Client
	access  string
	hub     string
	expires time.Time
}

func (s *Store) InstallationObserver(ctx context.Context, name string) (*InstallationObserver, error) {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return nil, err
	}
	client, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return nil, err
	}
	return &InstallationObserver{client: client, access: r.Tokens.AccessToken, hub: r.Tokens.Principal.ID, expires: r.Tokens.Principal.AccessExpiresAt}, nil
}
func (o *InstallationObserver) Close()        { o.client.Close() }
func (o *InstallationObserver) Expired() bool { return !o.expires.After(time.Now()) }
func (o *InstallationObserver) Read(ctx context.Context) ([]identity.ManagedIdentity, error) {
	if o.Expired() {
		return nil, relayclient.ErrCredentials
	}
	items, err := o.client.ManagedInstallations(ctx, o.access)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.HubID != o.hub || item.Kind != "installation" {
			return nil, relayclient.ErrRequest
		}
	}
	return items, nil
}
