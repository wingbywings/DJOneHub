package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type GrantStatus struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Modules    []string   `json:"modules"`
	State      string     `json:"state"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	IdentityID string     `json:"identity_id,omitempty"`
}

func (s *Store) ManagedPairings(ctx context.Context, token string) ([]GrantStatus, error) {
	p, err := s.Authenticate(ctx, token)
	if err != nil || p.Kind != "hub" {
		return nil, ErrUnauthorized
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM grant_metadata WHERE tenant=? AND hub_id=? AND kind='pairing' ORDER BY rowid DESC LIMIT 128", p.Tenant, p.ID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []GrantStatus{}
	for _, id := range ids {
		g, err := grantStatus(ctx, s.db, id, p.Tenant, p.ID, s.now())
		if err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, nil
}

func (s *Store) PairingStatus(ctx context.Context, token, id string) (GrantStatus, error) {
	p, err := s.Authenticate(ctx, token)
	if err != nil || p.Kind != "hub" {
		return GrantStatus{}, ErrUnauthorized
	}
	return grantStatus(ctx, s.db, id, p.Tenant, p.ID, s.now())
}
func grantStatus(ctx context.Context, q querier, id, tenant, hub string, now time.Time) (GrantStatus, error) {
	var g GrantStatus
	var modules string
	var created sql.NullInt64
	var expires int64
	var revoked, consumed bool
	err := q.QueryRowContext(ctx, "SELECT id,kind,name,modules,created,expires,revoked,consumed,identity_id FROM grant_metadata WHERE id=? AND tenant=? AND hub_id=?", id, tenant, hub).Scan(&g.ID, &g.Kind, &g.Name, &modules, &created, &expires, &revoked, &consumed, &g.IdentityID)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrUnauthorized
	}
	if err != nil {
		return g, err
	}
	if json.Unmarshal([]byte(modules), &g.Modules) != nil {
		return g, ErrInvalid
	}
	g.CreatedAt = optionalTime(created)
	g.ExpiresAt = time.Unix(expires, 0).UTC()
	g.State = "pending"
	switch {
	case consumed:
		g.State = "consumed"
	case revoked:
		g.State = "revoked"
	case expires <= now.Unix():
		g.State = "expired"
	}
	return g, nil
}
func optionalTime(t sql.NullInt64) *time.Time {
	if !t.Valid {
		return nil
	}
	v := time.Unix(t.Int64, 0).UTC()
	return &v
}
func (s *Store) RevokePairing(ctx context.Context, token, id string) error {
	if !validID(id) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := s.authenticate(ctx, tx, token)
	if err != nil || p.Kind != "hub" {
		return ErrUnauthorized
	}
	g, err := grantStatus(ctx, tx, id, p.Tenant, p.ID, s.now())
	if err != nil {
		return err
	}
	if g.Kind != "pairing" || g.State == "consumed" {
		return ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, "UPDATE grant_metadata SET revoked=1 WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
func validLabel(value string) bool {
	return len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}
func (s *Store) NamePairing(ctx context.Context, token, id, name string) error {
	if !validLabel(name) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := s.authenticate(ctx, tx, token)
	if err != nil || p.Kind != "hub" {
		return ErrUnauthorized
	}
	g, err := grantStatus(ctx, tx, id, p.Tenant, p.ID, s.now())
	if err != nil {
		return err
	}
	if g.Kind != "pairing" || g.State != "pending" {
		return ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, "UPDATE grant_metadata SET name=? WHERE id=?", name, id); err != nil {
		return err
	}
	return tx.Commit()
}

type ManagedIdentity struct {
	ID                string     `json:"id"`
	Tenant            string     `json:"tenant"`
	Kind              string     `json:"kind"`
	HubID             string     `json:"hub_id"`
	Name              string     `json:"name"`
	Modules           []string   `json:"modules"`
	Fingerprint       string     `json:"public_key_fingerprint"`
	Revoked           bool       `json:"revoked"`
	CreatedAt         *time.Time `json:"created_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	LastAuthenticated *time.Time `json:"last_authenticated,omitempty"`
	PushRegistered    bool       `json:"push_registered"`
	PushEnvironment   string     `json:"push_environment,omitempty"`
	SignalState       string     `json:"signal_state,omitempty"`
	SignalObservedAt  *time.Time `json:"signal_observed_at,omitempty"`
}

// Health checks the running process's identity database, without returning data.
func (s *Store) Health(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM identities LIMIT 1)").Scan(&one)
}

func (s *Store) managedIdentities(ctx context.Context, tenant, hub string) ([]ManagedIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,i.tenant,i.kind,i.hub_id,m.name,i.modules,i.public_key,(i.revoked OR h.revoked),m.created,m.revoked_at,m.last_authenticated,COALESCE(p.environment,'') FROM identities i JOIN identities h ON h.id=i.hub_id JOIN identity_metadata m ON m.id=i.id LEFT JOIN push_endpoints p ON p.identity_id=i.id WHERE i.tenant=? AND (?='' OR (i.hub_id=? AND i.kind='installation')) ORDER BY i.id LIMIT 1024`, tenant, hub, hub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ManagedIdentity{}
	for rows.Next() {
		var r ManagedIdentity
		var modules string
		var key []byte
		var created, revoked, last sql.NullInt64
		if err = rows.Scan(&r.ID, &r.Tenant, &r.Kind, &r.HubID, &r.Name, &modules, &key, &r.Revoked, &created, &revoked, &last, &r.PushEnvironment); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(modules), &r.Modules) != nil {
			return nil, ErrInvalid
		}
		r.Fingerprint = KeyFingerprint(key)
		r.CreatedAt = optionalTime(created)
		r.RevokedAt = optionalTime(revoked)
		r.LastAuthenticated = optionalTime(last)
		r.PushRegistered = r.PushEnvironment != ""
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) ManagedInstallations(ctx context.Context, token string) ([]ManagedIdentity, error) {
	p, err := s.Authenticate(ctx, token)
	if err != nil || p.Kind != "hub" {
		return nil, ErrUnauthorized
	}
	return s.managedIdentities(ctx, p.Tenant, p.ID)
}
func (s *Store) AdminIdentities(ctx context.Context, tenant string) ([]ManagedIdentity, error) {
	if !validID(tenant) {
		return nil, ErrInvalid
	}
	return s.managedIdentities(ctx, tenant, "")
}
func (s *Store) NameInstallation(ctx context.Context, token, id, name string) error {
	if !validID(id) || !validLabel(name) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := s.authenticate(ctx, tx, token)
	if err != nil || p.Kind != "hub" {
		return ErrUnauthorized
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE id=? AND tenant=? AND hub_id=? AND kind='installation' AND revoked=0", id, p.Tenant, p.ID).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrUnauthorized
	}
	if _, err = tx.ExecContext(ctx, "UPDATE identity_metadata SET name=? WHERE id=?", name, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AdminRevokeTenant(ctx context.Context, tenant, id string) error {
	if !validID(tenant) || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE id=? AND tenant=?", id, tenant).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrUnauthorized
	}
	if err = s.revokeIdentities(ctx, tx, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.notifyRevocation()
	return nil
}

// AdminDeleteRevokedTenant removes revoked identities and their credentials.
// A revoked Hub includes its descendants; enrollment/grant metadata is retained
// as historical evidence, so deletion cannot make a consumed grant reusable.
func (s *Store) AdminDeleteRevokedTenant(ctx context.Context, tenant, id string) (int64, error) {
	if !validID(tenant) || !validID(id) {
		return 0, ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var kind string
	var revoked, hubRevoked bool
	err = tx.QueryRowContext(ctx, `SELECT i.kind,i.revoked,h.revoked FROM identities i JOIN identities h ON h.id=i.hub_id WHERE i.id=? AND i.tenant=?`, id, tenant).Scan(&kind, &revoked, &hubRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUnauthorized
	}
	if err != nil {
		return 0, err
	}
	if !revoked && !hubRevoked {
		return 0, ErrInvalid
	}
	// Reject inconsistent cross-tenant descendants rather than orphan them.
	if kind == "hub" {
		var foreign int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE hub_id=? AND tenant<>?", id, tenant).Scan(&foreign); err != nil {
			return 0, err
		}
		if foreign != 0 {
			return 0, ErrInvalid
		}
	}
	where := `WHERE tenant=? AND (id=? OR (?='hub' AND hub_id=?))`
	targets := "SELECT id FROM identities " + where
	args := []any{tenant, id, kind, id}
	if _, err = tx.ExecContext(ctx, "UPDATE grant_metadata SET revoked=1 WHERE consumed=0 AND hub_id IN ("+targets+")", args...); err != nil {
		return 0, err
	}
	grantArgs := append(append([]any{}, args...), args...)
	if _, err = tx.ExecContext(ctx, "DELETE FROM grants WHERE hub_id IN ("+targets+") OR id IN (SELECT id FROM grant_metadata WHERE identity_id IN ("+targets+"))", grantArgs...); err != nil {
		return 0, err
	}
	// These are fixed internal table names; all user input remains bound values.
	for _, table := range []string{"access_tokens", "refresh_used", "challenges", "push_endpoints", "identity_metadata"} {
		column := "identity_id"
		if table == "identity_metadata" {
			column = "id"
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+column+" IN ("+targets+")", args...); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM identities "+where, args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	s.notifyRevocation()
	return count, nil
}

func (s *Store) revokeIdentities(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE identity_metadata SET revoked_at=? WHERE id IN (SELECT id FROM identities WHERE (id=? OR hub_id=?) AND revoked=0)", s.now().Unix(), id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE identities SET revoked=1,version=version+1 WHERE (id=? OR hub_id=?) AND revoked=0", id, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE grant_metadata SET revoked=1 WHERE hub_id=? AND consumed=0", id)
	return err
}
