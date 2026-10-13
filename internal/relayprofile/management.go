package relayprofile

import (
	"context"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func (s *Store) management(ctx context.Context, name string, run func(*relayclient.Client, string) error) error {
	r, err := s.access(ctx, name, false)
	if err != nil {
		return err
	}
	api, err := relayclient.New(r.RelayURL, s.options)
	if err != nil {
		return err
	}
	defer api.Close()
	return run(api, r.Tokens.AccessToken)
}
func (s *Store) ManagedInstallations(ctx context.Context, name string) ([]identity.ManagedIdentity, error) {
	var items []identity.ManagedIdentity
	err := s.management(ctx, name, func(api *relayclient.Client, access string) error {
		var err error
		items, err = api.ManagedInstallations(ctx, access)
		return err
	})
	return items, err
}
func (s *Store) PairingStatus(ctx context.Context, name, id string) (identity.GrantStatus, error) {
	var status identity.GrantStatus
	err := s.management(ctx, name, func(api *relayclient.Client, access string) error {
		var err error
		status, err = api.PairingStatus(ctx, access, id)
		return err
	})
	return status, err
}
func (s *Store) ManagedPairings(ctx context.Context, name string) ([]identity.GrantStatus, error) {
	var items []identity.GrantStatus
	err := s.management(ctx, name, func(api *relayclient.Client, access string) error {
		var err error
		items, err = api.ManagedPairings(ctx, access)
		return err
	})
	return items, err
}
func (s *Store) RevokePairing(ctx context.Context, name, id string) error {
	return s.management(ctx, name, func(api *relayclient.Client, access string) error { return api.RevokePairing(ctx, access, id) })
}
func (s *Store) NameManaged(ctx context.Context, name, kind, id, label string) error {
	return s.management(ctx, name, func(api *relayclient.Client, access string) error {
		return api.NameManaged(ctx, access, kind, id, label)
	})
}

// Remove only deletes the local vault entry. It does not revoke remote Hub or
// child identities. The caller must first disconnect it and check call ownership.
func (s *Store) Remove(ctx context.Context, name string) error {
	unlock, err := s.lockContext(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	deleter, ok := s.vault.(interface{ Delete(string) error })
	if !ok {
		return ErrUnsupported
	}
	if _, err := s.vault.Load(name); err != nil {
		return err
	}
	return deleter.Delete(name)
}
