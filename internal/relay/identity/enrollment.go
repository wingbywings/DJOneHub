package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// EnrollmentRequest contains public information only. A signature proves key
// possession, not administrative authority or the physical existence of a modem.
type ModuleChoice struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}
type EnrollmentRequest struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	RelayURL  string         `json:"relay_url"`
	PublicKey []byte         `json:"public_key"`
	Modules   []ModuleChoice `json:"modules"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
	Signature []byte         `json:"signature"`
}

func EnrollmentRequestMessage(r EnrollmentRequest) []byte {
	data, _ := json.Marshal(struct {
		Domain    string         `json:"domain"`
		Version   int            `json:"version"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		RelayURL  string         `json:"relay_url"`
		PublicKey []byte         `json:"public_key"`
		Modules   []ModuleChoice `json:"modules"`
		CreatedAt time.Time      `json:"created_at"`
		ExpiresAt time.Time      `json:"expires_at"`
	}{"DJOneHub/identity/enrollment-request/v1", r.Version, r.ID, r.Name, r.RelayURL, r.PublicKey, r.Modules, r.CreatedAt, r.ExpiresAt})
	return data
}
func HTTPSOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && u.Opaque == "" && !strings.HasSuffix(u.Host, ":")
}
func (r EnrollmentRequest) Validate(now time.Time) error {
	if r.Version != 1 || !validID(r.ID) || !strings.HasPrefix(r.ID, "request_") || len(r.ID) < 30 || r.Name == "" || len(r.Name) > 128 || !utf8.ValidString(r.Name) || !validLabel(r.Name) || !HTTPSOrigin(r.RelayURL) || len(r.PublicKey) != ed25519.PublicKeySize || len(r.Modules) == 0 || len(r.Modules) > 32 || r.CreatedAt.After(now.Add(30*time.Second)) || r.CreatedAt.Before(now.Add(-30*time.Minute)) || !r.ExpiresAt.After(now) || r.ExpiresAt.After(r.CreatedAt.Add(30*time.Minute)) || !r.ExpiresAt.After(r.CreatedAt) {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, m := range r.Modules {
		if !validID(m.ID) || seen[m.ID] || len(m.Alias) > 320 || !utf8.ValidString(m.Alias) || utf8.RuneCountInString(m.Alias) > 80 {
			return ErrInvalid
		}
		seen[m.ID] = true
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16384 {
		return ErrInvalid
	}
	if !ed25519.Verify(r.PublicKey, EnrollmentRequestMessage(r), r.Signature) {
		return ErrUnauthorized
	}
	return nil
}

type EnrollmentRequestStatus struct {
	Request         EnrollmentRequest `json:"request"`
	State           string            `json:"state"`
	Tenant          string            `json:"tenant,omitempty"`
	ApprovedModules []string          `json:"approved_modules,omitempty"`
	HubID           string            `json:"hub_id,omitempty"`
	Authorization   *GrantStatus      `json:"authorization,omitempty"`
}
type EnrollmentAuthorization struct {
	Version              int    `json:"version"`
	RequestID            string `json:"request_id"`
	RelayURL             string `json:"relay_url"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
	Grant                Grant  `json:"grant"`
}

func KeyFingerprint(key []byte) string { sum := sha256.Sum256(key); return hex.EncodeToString(sum[:]) }

func (s *Store) SubmitEnrollmentRequest(ctx context.Context, r EnrollmentRequest) (EnrollmentRequestStatus, error) {
	if err := r.Validate(s.now()); err != nil {
		return EnrollmentRequestStatus{}, err
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return EnrollmentRequestStatus{}, err
	}
	defer tx.Rollback()
	data, err := json.Marshal(r)
	if err != nil {
		return EnrollmentRequestStatus{}, ErrInvalid
	}
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT payload FROM enrollment_requests WHERE id=?", r.ID).Scan(&existing)
	if err == nil {
		if existing != string(data) {
			return EnrollmentRequestStatus{}, ErrUnauthorized
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM enrollment_requests WHERE state IN ('pending','approved') AND expires>?", s.now().Unix()).Scan(&count); err != nil {
			return EnrollmentRequestStatus{}, err
		}
		if count >= 256 {
			return EnrollmentRequestStatus{}, ErrCapacity
		}
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM enrollment_requests WHERE public_key=? AND state IN ('pending','approved') AND expires>?", r.PublicKey, s.now().Unix()).Scan(&count); err != nil {
			return EnrollmentRequestStatus{}, err
		}
		if count >= 3 {
			return EnrollmentRequestStatus{}, ErrCapacity
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO enrollment_requests(id,payload,public_key,state,expires) VALUES(?,?,?,'pending',?)", r.ID, string(data), r.PublicKey, r.ExpiresAt.Unix())
		if err != nil {
			return EnrollmentRequestStatus{}, err
		}
	} else {
		return EnrollmentRequestStatus{}, err
	}
	result, err := requestStatus(ctx, tx, r.ID, s.now())
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
func requestStatus(ctx context.Context, q querier, id string, now time.Time) (EnrollmentRequestStatus, error) {
	var r EnrollmentRequestStatus
	var payload, approved string
	var expiry int64
	err := q.QueryRowContext(ctx, "SELECT payload,state,tenant,approved_modules,hub_id,expires FROM enrollment_requests WHERE id=?", id).Scan(&payload, &r.State, &r.Tenant, &approved, &r.HubID, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrInvalid
	}
	if err != nil {
		return r, err
	}
	if json.Unmarshal([]byte(payload), &r.Request) != nil || json.Unmarshal([]byte(approved), &r.ApprovedModules) != nil {
		return r, ErrInvalid
	}
	if (r.State == "pending" || r.State == "approved") && expiry <= now.Unix() {
		r.State = "expired"
	}
	var grantID string
	err = q.QueryRowContext(ctx, "SELECT id FROM grant_metadata WHERE request_id=? ORDER BY rowid DESC LIMIT 1", id).Scan(&grantID)
	if err == nil {
		g, err := grantStatus(ctx, q, grantID, r.Tenant, "", now)
		if err != nil {
			return r, err
		}
		r.Authorization = &g
	} else if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	return r, nil
}

// These Admin methods require an authenticated, tenant-scoped administration
// boundary. They are not registered on the public/native identity router.
func (s *Store) AdminEnrollmentRequests(ctx context.Context, tenant string) ([]EnrollmentRequestStatus, error) {
	if !validID(tenant) {
		return nil, ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id FROM enrollment_requests WHERE tenant='' OR tenant=? ORDER BY rowid DESC LIMIT 256", tenant)
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
	result := []EnrollmentRequestStatus{}
	for _, id := range ids {
		r, err := requestStatus(ctx, tx, id, s.now())
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, tx.Commit()
}
func (s *Store) AdminApproveEnrollment(ctx context.Context, tenant, id string, modules []string) error {
	if !validID(tenant) || !validID(id) {
		return ErrInvalid
	}
	modules, err := normalizeModules(modules)
	if err != nil {
		return err
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := requestStatus(ctx, tx, id, s.now())
	if err != nil {
		return err
	}
	if r.Tenant != "" && r.Tenant != tenant {
		return ErrUnauthorized
	}
	if r.State != "pending" && r.State != "approved" {
		return ErrInvalid
	}
	for _, module := range modules {
		found := false
		for _, m := range r.Request.Modules {
			found = found || module == m.ID
		}
		if !found {
			return ErrUnauthorized
		}
	}
	encoded, _ := json.Marshal(modules)
	if r.State == "approved" {
		existing, _ := json.Marshal(r.ApprovedModules)
		if string(existing) != string(encoded) {
			return ErrInvalid
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, "UPDATE enrollment_requests SET state='approved',tenant=?,approved_modules=? WHERE id=?", tenant, string(encoded), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AdminIssueEnrollment(ctx context.Context, tenant, id, origin string) (EnrollmentAuthorization, error) {
	if !validID(tenant) || !validID(id) || !HTTPSOrigin(origin) {
		return EnrollmentAuthorization{}, ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return EnrollmentAuthorization{}, err
	}
	defer tx.Rollback()
	r, err := requestStatus(ctx, tx, id, s.now())
	if err != nil {
		return EnrollmentAuthorization{}, err
	}
	if r.Tenant != tenant {
		return EnrollmentAuthorization{}, ErrUnauthorized
	}
	if r.State != "approved" || strings.TrimSuffix(r.Request.RelayURL, "/") != strings.TrimSuffix(origin, "/") {
		return EnrollmentAuthorization{}, ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, "UPDATE grant_metadata SET revoked=1 WHERE request_id=? AND identity_id=''", id); err != nil {
		return EnrollmentAuthorization{}, err
	}
	// The signature and approval remain fixed. Re-download issues a fresh grant
	// atomically; a prior unconsumed file is no longer redeemable.
	lifetime := min(10*time.Minute, r.Request.ExpiresAt.Sub(s.now()))
	g, err := s.insertGrant(ctx, tx, "enrollment", tenant, "", r.ApprovedModules, lifetime)
	if err != nil {
		return EnrollmentAuthorization{}, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE grant_metadata SET request_id=?,bound_key=? WHERE id=?", id, r.Request.PublicKey, g.ID)
	if err != nil {
		return EnrollmentAuthorization{}, err
	}
	result := EnrollmentAuthorization{2, id, strings.TrimSuffix(origin, "/"), KeyFingerprint(r.Request.PublicKey), g}
	return result, tx.Commit()
}
func (s *Store) AdminRejectEnrollment(ctx context.Context, tenant, id string) error {
	if !validID(tenant) || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := requestStatus(ctx, tx, id, s.now())
	if err != nil {
		return err
	}
	if r.Tenant != "" && r.Tenant != tenant {
		return ErrUnauthorized
	}
	if r.State == "completed" {
		return ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, "UPDATE enrollment_requests SET state='rejected',tenant=? WHERE id=?", tenant, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE grant_metadata SET revoked=1 WHERE request_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminDeleteEnrollment removes the request and every associated authorization.
// Registered identities are independent of these records and remain valid.
func (s *Store) AdminDeleteEnrollment(ctx context.Context, tenant, id string) (int64, error) {
	if !validID(tenant) || !validID(id) {
		return 0, ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := requestStatus(ctx, tx, id, s.now())
	if err != nil {
		return 0, err
	}
	if r.Tenant != "" && r.Tenant != tenant {
		return 0, ErrUnauthorized
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM grants WHERE id IN (SELECT id FROM grant_metadata WHERE request_id=?)", id); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM grant_metadata WHERE request_id=?", id)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM enrollment_requests WHERE id=?", id); err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

// AdminDeleteEnrollmentAuthorization deletes one file's server-side grant;
// possession of a previously downloaded copy no longer permits enrollment.
func (s *Store) AdminDeleteEnrollmentAuthorization(ctx context.Context, tenant, id, grantID string) error {
	if !validID(tenant) || !validID(id) || !validID(grantID) {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := requestStatus(ctx, tx, id, s.now())
	if err != nil {
		return err
	}
	if r.Tenant != tenant {
		return ErrUnauthorized
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM grant_metadata WHERE id=? AND request_id=? AND tenant=? AND kind='enrollment'", grantID, id, tenant).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrUnauthorized
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM grants WHERE id=?", grantID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM grant_metadata WHERE id=?", grantID); err != nil {
		return err
	}
	return tx.Commit()
}
