package identity

import (
	"context"
	"encoding/json"
	"github.com/iniwex5/vohive/internal/voippush"
	"time"
)

// Push addresses are private routing data, never credentials or public identity metadata.
type PushEndpoint struct {
	IdentityID, Environment, Token, Registration string
	alert                                        bool
	Updated                                      time.Time
}

func (s *Store) SetPushEndpoint(ctx context.Context, access, environment, token string) error {
	return s.setPushEndpoint(ctx, access, environment, token, false)
}

func (s *Store) SetAlertPushEndpoint(ctx context.Context, access, environment, token string) error {
	return s.setPushEndpoint(ctx, access, environment, token, true)
}

func pushTable(alert bool) string {
	if alert {
		return "alert_push_endpoints"
	}
	return "push_endpoints"
}

func (s *Store) setPushEndpoint(ctx context.Context, access, environment, token string, alert bool) error {
	table := pushTable(alert)
	if environment != "sandbox" && environment != "production" || token != "" && !voippush.ValidToken(token) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := s.authenticate(ctx, tx, access)
	if err != nil {
		return err
	}
	if p.Kind != "installation" {
		return ErrUnauthorized
	}
	if token == "" {
		_, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE identity_id=?", p.ID)
	} else {
		// Allow a re-paired installation to reclaim an address only after the old identity was revoked/expired.
		_, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE identity_id IN (SELECT i.id FROM identities i JOIN identities h ON h.id=i.hub_id WHERE i.revoked=1 OR h.revoked=1 OR i.refresh_expires<=?)`, s.now().Unix())
		if err != nil {
			return err
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE environment=? AND token=? AND identity_id<>?", environment, token, p.ID).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrInvalid
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO `+table+` VALUES(?,?,?,?,?) ON CONFLICT(identity_id) DO UPDATE SET environment=excluded.environment,token=excluded.token,registration=excluded.registration,updated=excluded.updated`, p.ID, environment, token, randomToken(16), s.now().UnixMilli())
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) PushEndpoint(ctx context.Context, hubToken, id, module string) (PushEndpoint, error) {
	return s.pushEndpoint(ctx, hubToken, id, module, false)
}
func (s *Store) AlertPushEndpoint(ctx context.Context, hubToken, id, module string) (PushEndpoint, error) {
	return s.pushEndpoint(ctx, hubToken, id, module, true)
}
func (s *Store) pushEndpoint(ctx context.Context, hubToken, id, module string, alert bool) (PushEndpoint, error) {
	table := pushTable(alert)
	tx, err := s.transaction(ctx)
	if err != nil {
		return PushEndpoint{}, err
	}
	defer tx.Rollback()
	hub, err := s.authenticate(ctx, tx, hubToken)
	if err != nil || hub.Kind != "hub" || !contains(hub.Modules, module) {
		return PushEndpoint{}, ErrUnauthorized
	}
	var e PushEndpoint
	var modules string
	var updated int64
	err = tx.QueryRowContext(ctx, `SELECT p.identity_id,p.environment,p.token,p.registration,p.updated,i.modules FROM `+table+` p JOIN identities i ON i.id=p.identity_id WHERE i.id=? AND i.hub_id=? AND i.tenant=? AND i.kind='installation' AND i.revoked=0 AND i.refresh_expires>?`, id, hub.ID, hub.Tenant, s.now().Unix()).Scan(&e.IdentityID, &e.Environment, &e.Token, &e.Registration, &updated, &modules)
	if err != nil {
		return PushEndpoint{}, ErrUnauthorized
	}
	var allowed []string
	if json.Unmarshal([]byte(modules), &allowed) != nil || !contains(allowed, module) {
		return PushEndpoint{}, ErrUnauthorized
	}
	e.alert = alert
	e.Updated = time.UnixMilli(updated)
	return e, nil
}
func (s *Store) InvalidatePushEndpoint(ctx context.Context, e PushEndpoint, at time.Time) error {
	table := pushTable(e.alert)
	if !at.IsZero() && at.Before(e.Updated) {
		return nil
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE identity_id=? AND registration=?", e.IdentityID, e.Registration)
	return err
}
