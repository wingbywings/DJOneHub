// Package identity owns the relay's tenant, Hub and installation identities.
// APNs addresses are deliberately not authentication credentials.
package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrUnauthorized = errors.New("invalid, expired or revoked credential")
	ErrInvalid      = errors.New("invalid identity request")
	ErrCapacity     = errors.New("identity request limit reached")
	ErrRefreshReuse = errors.New("refresh credential reused; identity revoked")
)

const AccessLifetime = 15 * time.Minute
const RefreshLifetime = 30 * 24 * time.Hour

type Store struct {
	db      *sql.DB
	now     func() time.Time
	mu      sync.Mutex
	changed chan struct{}
}
type Principal struct {
	ID              string    `json:"id"`
	Tenant          string    `json:"tenant"`
	Kind            string    `json:"kind"`
	HubID           string    `json:"hub_id"`
	Modules         []string  `json:"modules"`
	Version         uint64    `json:"version"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
}
type Grant struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
	Modules   []string  `json:"modules"`
}
type GrantProof struct {
	ID        string `json:"id"`
	Secret    string `json:"secret"`
	PublicKey []byte `json:"public_key"`
	Signature []byte `json:"signature"`
}
type Tokens struct {
	Principal        Principal `json:"principal"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}
type Installation struct {
	ID      string   `json:"id"`
	Modules []string `json:"modules"`
}
type Challenge struct {
	ID        string    `json:"id"`
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expires_at"`
}
type RefreshProof struct {
	IdentityID   string `json:"identity_id"`
	RefreshToken string `json:"refresh_token"`
	ChallengeID  string `json:"challenge_id"`
	Challenge    string `json:"challenge"`
	Signature    []byte `json:"signature"`
}

// Signing messages are versioned canonical JSON emitted by these functions.
// Mobile clients must use exactly this field order, UTF-8 and standard base64
// public keys; no whitespace. Challenges bind proof to a single operation.
func GrantMessage(kind string, proof GrantProof) []byte {
	data, _ := json.Marshal(struct {
		Domain string `json:"domain"`
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Secret string `json:"secret"`
		Key    []byte `json:"public_key"`
	}{"DJOneHub/identity/grant/v1", kind, proof.ID, proof.Secret, proof.PublicKey})
	return data
}
func RefreshMessage(proof RefreshProof) []byte {
	data, _ := json.Marshal(struct {
		Domain      string `json:"domain"`
		ID          string `json:"identity_id"`
		Token       string `json:"refresh_token"`
		ChallengeID string `json:"challenge_id"`
		Challenge   string `json:"challenge"`
	}{"DJOneHub/identity/refresh/v1", proof.IdentityID, proof.RefreshToken, proof.ChallengeID, proof.Challenge})
	return data
}
func randomToken(size int) string {
	b := make([]byte, size)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
func equal(a, b string) bool   { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func normalizeModules(modules []string) ([]string, error) {
	if len(modules) == 0 || len(modules) > 32 {
		return nil, ErrInvalid
	}
	modules = append([]string(nil), modules...)
	sort.Strings(modules)
	for i, id := range modules {
		if !validID(id) || i > 0 && modules[i-1] == id {
			return nil, ErrInvalid
		}
	}
	return modules, nil
}

func Open(directory string) (*Store, error) {
	if !filepath.IsAbs(directory) {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("relay state directory must be private (0700)")
	}
	path := filepath.Join(directory, "identities.sqlite")
	// Refuse symlinks for the DB and SQLite's adjacent journal files.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if file, err := os.Lstat(path + suffix); err == nil {
			if !file.Mode().IsRegular() || file.Mode().Perm()&0077 != 0 {
				return nil, errors.New("unsafe relay state file")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	file.Close()
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now, changed: make(chan struct{})}
	if err := s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) initialize() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 2 {
		return errors.New("unsupported relay identity schema")
	}
	if version == 0 {
		_, err = tx.Exec(`
CREATE TABLE identities(id TEXT PRIMARY KEY, tenant TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('hub','installation')), hub_id TEXT NOT NULL, public_key BLOB NOT NULL, modules TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, revoked INTEGER NOT NULL DEFAULT 0, refresh_hash TEXT NOT NULL UNIQUE, refresh_expires INTEGER NOT NULL, UNIQUE(hub_id, kind, public_key));
CREATE TABLE grants(id TEXT PRIMARY KEY, kind TEXT NOT NULL, tenant TEXT NOT NULL, hub_id TEXT NOT NULL, secret_hash TEXT NOT NULL, modules TEXT NOT NULL, expires INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE access_tokens(hash TEXT PRIMARY KEY, identity_id TEXT NOT NULL REFERENCES identities(id), version INTEGER NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE refresh_used(hash TEXT PRIMARY KEY, identity_id TEXT NOT NULL REFERENCES identities(id), expires INTEGER NOT NULL);
CREATE TABLE challenges(id TEXT PRIMARY KEY, identity_id TEXT NOT NULL REFERENCES identities(id), value_hash TEXT NOT NULL, refresh_hash TEXT NOT NULL, expires INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0);
CREATE INDEX grant_owner ON grants(hub_id, expires);
CREATE INDEX challenge_owner ON challenges(identity_id, expires);
CREATE INDEX access_expiry ON access_tokens(expires);
PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = tx.Exec(`CREATE TABLE push_endpoints(identity_id TEXT PRIMARY KEY REFERENCES identities(id), environment TEXT NOT NULL, token TEXT NOT NULL, registration TEXT NOT NULL, updated INTEGER NOT NULL, UNIQUE(environment,token)); PRAGMA user_version=2;`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) transaction(ctx context.Context) (*sql.Tx, error) { return s.db.BeginTx(ctx, nil) }
func prune(ctx context.Context, tx *sql.Tx, now int64) error {
	for _, statement := range []string{"DELETE FROM access_tokens WHERE expires<=?", "DELETE FROM challenges WHERE expires<=?", "DELETE FROM refresh_used WHERE expires<=?", "DELETE FROM grants WHERE expires<=?"} {
		if _, err := tx.ExecContext(ctx, statement, now); err != nil {
			return err
		}
	}
	return nil
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) authenticate(ctx context.Context, q querier, token string) (Principal, error) {
	if len(token) != 43 {
		return Principal{}, ErrUnauthorized
	}
	var p Principal
	var modules string
	var expires int64
	err := q.QueryRowContext(ctx, `SELECT i.id,i.tenant,i.kind,i.hub_id,i.modules,i.version,a.expires FROM access_tokens a JOIN identities i ON i.id=a.identity_id JOIN identities h ON h.id=i.hub_id WHERE a.hash=? AND a.expires>? AND a.version=i.version AND i.revoked=0 AND h.revoked=0`, hash(token), s.now().Unix()).Scan(&p.ID, &p.Tenant, &p.Kind, &p.HubID, &modules, &p.Version, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	if err := json.Unmarshal([]byte(modules), &p.Modules); err != nil {
		return Principal{}, err
	}
	p.AccessExpiresAt = time.Unix(expires, 0).UTC()
	return p, nil
}
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	return s.authenticate(ctx, s.db, token)
}

// ProvisionEnrollment is an offline administration operation, never an
// unauthenticated HTTP endpoint. It creates exactly one limited Hub grant.
func (s *Store) ProvisionEnrollment(ctx context.Context, tenant string, modules []string) (Grant, error) {
	if !validID(tenant) {
		return Grant{}, ErrInvalid
	}
	modules, err := normalizeModules(modules)
	if err != nil {
		return Grant{}, err
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback()
	grant, err := s.insertGrant(ctx, tx, "enrollment", tenant, "", modules, 10*time.Minute)
	if err != nil {
		return Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, err
	}
	return grant, nil
}
func (s *Store) insertGrant(ctx context.Context, tx *sql.Tx, kind, tenant, hubID string, modules []string, lifetime time.Duration) (Grant, error) {
	if err := prune(ctx, tx, s.now().Unix()); err != nil {
		return Grant{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM grants WHERE hub_id=? AND consumed=0", hubID).Scan(&count); err != nil {
		return Grant{}, err
	}
	if count >= 16 {
		return Grant{}, ErrCapacity
	}
	g := Grant{ID: "grant_" + randomToken(16), Secret: randomToken(32), ExpiresAt: s.now().Add(lifetime).UTC(), Modules: modules}
	data, _ := json.Marshal(modules)
	_, err := tx.ExecContext(ctx, "INSERT INTO grants(id,kind,tenant,hub_id,secret_hash,modules,expires) VALUES(?,?,?,?,?,?,?)", g.ID, kind, tenant, hubID, hash(g.Secret), string(data), g.ExpiresAt.Unix())
	return g, err
}
func (s *Store) CreatePairing(ctx context.Context, accessToken string, modules []string) (Grant, error) {
	modules, err := normalizeModules(modules)
	if err != nil {
		return Grant{}, err
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback()
	hub, err := s.authenticate(ctx, tx, accessToken)
	if err != nil {
		return Grant{}, err
	}
	if hub.Kind != "hub" {
		return Grant{}, ErrUnauthorized
	}
	for _, module := range modules {
		if !contains(hub.Modules, module) {
			return Grant{}, ErrUnauthorized
		}
	}
	g, err := s.insertGrant(ctx, tx, "pairing", hub.Tenant, hub.ID, modules, 5*time.Minute)
	if err != nil {
		return Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, err
	}
	return g, nil
}
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
func (s *Store) Enroll(ctx context.Context, proof GrantProof) (Tokens, error) {
	return s.redeem(ctx, "enrollment", proof)
}
func (s *Store) Redeem(ctx context.Context, proof GrantProof) (Tokens, error) {
	return s.redeem(ctx, "pairing", proof)
}
func (s *Store) redeem(ctx context.Context, kind string, proof GrantProof) (Tokens, error) {
	if !validID(proof.ID) || len(proof.Secret) != 43 || len(proof.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(proof.PublicKey, GrantMessage(kind, proof), proof.Signature) {
		return Tokens{}, ErrUnauthorized
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback()
	var tenant, hubID, storedHash, modules string
	err = tx.QueryRowContext(ctx, "SELECT tenant,hub_id,secret_hash,modules FROM grants WHERE id=? AND kind=? AND expires>? AND consumed=0", proof.ID, kind, s.now().Unix()).Scan(&tenant, &hubID, &storedHash, &modules)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !equal(storedHash, hash(proof.Secret)) {
		return Tokens{}, ErrUnauthorized
	}
	if err != nil {
		return Tokens{}, err
	}
	identityKind, prefix := "installation", "ios_"
	if kind == "enrollment" {
		identityKind, prefix = "hub", "hub_"
	} else {
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE id=? AND tenant=? AND kind='hub' AND revoked=0", hubID, tenant).Scan(&active); err != nil {
			return Tokens{}, err
		}
		if active != 1 {
			return Tokens{}, ErrUnauthorized
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE hub_id=? AND kind='installation' AND revoked=0", hubID).Scan(&count); err != nil {
			return Tokens{}, err
		}
		if count >= 64 {
			return Tokens{}, ErrCapacity
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE hub_id=? AND kind='installation' AND public_key=?", hubID, proof.PublicKey).Scan(&count); err != nil {
			return Tokens{}, err
		}
		if count != 0 {
			return Tokens{}, ErrInvalid
		}
	}
	id := prefix + randomToken(16)
	if kind == "enrollment" {
		hubID = id
	}
	refresh := randomToken(32)
	refreshExpiry := s.now().Add(RefreshLifetime)
	_, err = tx.ExecContext(ctx, "INSERT INTO identities(id,tenant,kind,hub_id,public_key,modules,refresh_hash,refresh_expires) VALUES(?,?,?,?,?,?,?,?)", id, tenant, identityKind, hubID, proof.PublicKey, modules, hash(refresh), refreshExpiry.Unix())
	if err != nil {
		return Tokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE grants SET consumed=1 WHERE id=?", proof.ID); err != nil {
		return Tokens{}, err
	}
	tokens, err := s.issue(ctx, tx, id, refresh, refreshExpiry)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}
func (s *Store) issue(ctx context.Context, tx *sql.Tx, id, refresh string, refreshExpiry time.Time) (Tokens, error) {
	access := randomToken(32)
	if _, err := tx.ExecContext(ctx, "INSERT INTO access_tokens(hash,identity_id,version,expires) SELECT ?,id,version,? FROM identities WHERE id=?", hash(access), s.now().Add(AccessLifetime).Unix(), id); err != nil {
		return Tokens{}, err
	}
	p, err := s.authenticate(ctx, tx, access)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{p, access, refresh, refreshExpiry.UTC()}, nil
}

func (s *Store) Challenge(ctx context.Context, id, refresh string) (Challenge, error) {
	if !validID(id) || len(refresh) != 43 {
		return Challenge{}, ErrUnauthorized
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback()
	if err := prune(ctx, tx, s.now().Unix()); err != nil {
		return Challenge{}, err
	}
	var valid int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities i JOIN identities h ON h.id=i.hub_id WHERE i.id=? AND i.revoked=0 AND h.revoked=0 AND i.refresh_expires>? AND (i.refresh_hash=? OR EXISTS(SELECT 1 FROM refresh_used u WHERE u.identity_id=i.id AND u.hash=? AND u.expires>?))`, id, s.now().Unix(), hash(refresh), hash(refresh), s.now().Unix()).Scan(&valid)
	if err != nil {
		return Challenge{}, err
	}
	if valid != 1 {
		return Challenge{}, ErrUnauthorized
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM challenges WHERE identity_id=? AND consumed=0", id).Scan(&count); err != nil {
		return Challenge{}, err
	}
	if count >= 8 {
		return Challenge{}, ErrCapacity
	}
	c := Challenge{"challenge_" + randomToken(16), randomToken(32), s.now().Add(time.Minute).UTC()}
	_, err = tx.ExecContext(ctx, "INSERT INTO challenges(id,identity_id,value_hash,refresh_hash,expires) VALUES(?,?,?,?,?)", c.ID, id, hash(c.Value), hash(refresh), c.ExpiresAt.Unix())
	if err != nil {
		return Challenge{}, err
	}
	if err := tx.Commit(); err != nil {
		return Challenge{}, err
	}
	return c, nil
}
func (s *Store) Refresh(ctx context.Context, proof RefreshProof) (Tokens, error) {
	if !validID(proof.IdentityID) || !validID(proof.ChallengeID) || len(proof.RefreshToken) != 43 || len(proof.Challenge) != 43 {
		return Tokens{}, ErrUnauthorized
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback()
	var key []byte
	var currentHash, challengeHash, refreshHash string
	var expiry int64
	err = tx.QueryRowContext(ctx, `SELECT i.public_key,i.refresh_hash,i.refresh_expires,c.value_hash,c.refresh_hash FROM identities i JOIN identities h ON h.id=i.hub_id JOIN challenges c ON c.identity_id=i.id WHERE i.id=? AND i.revoked=0 AND h.revoked=0 AND i.refresh_expires>? AND c.id=? AND c.expires>? AND c.consumed=0`, proof.IdentityID, s.now().Unix(), proof.ChallengeID, s.now().Unix()).Scan(&key, &currentHash, &expiry, &challengeHash, &refreshHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, ErrUnauthorized
	}
	if err != nil {
		return Tokens{}, err
	}
	if !equal(challengeHash, hash(proof.Challenge)) || !equal(refreshHash, hash(proof.RefreshToken)) || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, RefreshMessage(proof), proof.Signature) {
		return Tokens{}, ErrUnauthorized
	}
	if _, err = tx.ExecContext(ctx, "UPDATE challenges SET consumed=1 WHERE id=?", proof.ChallengeID); err != nil {
		return Tokens{}, err
	}
	if !equal(currentHash, refreshHash) {
		if _, err = tx.ExecContext(ctx, "UPDATE identities SET revoked=1,version=version+1 WHERE id=?", proof.IdentityID); err != nil {
			return Tokens{}, err
		}
		if err := tx.Commit(); err != nil {
			return Tokens{}, err
		}
		s.notifyRevocation()
		return Tokens{}, ErrRefreshReuse
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO refresh_used(hash,identity_id,expires) VALUES(?,?,?)", currentHash, proof.IdentityID, expiry); err != nil {
		return Tokens{}, err
	}
	refresh := randomToken(32)
	refreshExpiry := s.now().Add(RefreshLifetime)
	if _, err = tx.ExecContext(ctx, "UPDATE identities SET refresh_hash=?,refresh_expires=? WHERE id=?", hash(refresh), refreshExpiry.Unix(), proof.IdentityID); err != nil {
		return Tokens{}, err
	}
	tokens, err := s.issue(ctx, tx, proof.IdentityID, refresh, refreshExpiry)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

func (s *Store) RevokeInstallation(ctx context.Context, hubToken, installationID string) error {
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hub, err := s.authenticate(ctx, tx, hubToken)
	if err != nil {
		return err
	}
	if hub.Kind != "hub" {
		return ErrUnauthorized
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE id=? AND kind='installation' AND hub_id=? AND tenant=?", installationID, hub.ID, hub.Tenant).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return ErrUnauthorized
	}
	if _, err := tx.ExecContext(ctx, "UPDATE identities SET revoked=1,version=version+1 WHERE id=? AND revoked=0", installationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyRevocation()
	return nil
}

func (s *Store) ListInstallations(ctx context.Context, hubToken string) ([]Installation, error) {
	tx, err := s.transaction(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	hub, err := s.authenticate(ctx, tx, hubToken)
	if err != nil {
		return nil, err
	}
	if hub.Kind != "hub" {
		return nil, ErrUnauthorized
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,modules FROM identities WHERE hub_id=? AND tenant=? AND kind='installation' AND revoked=0 ORDER BY id", hub.ID, hub.Tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Installation, 0)
	for rows.Next() {
		var item Installation
		var modules string
		if err := rows.Scan(&item.ID, &modules); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(modules), &item.Modules); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// AuthorizeModule always rereads the live credential/revocation state. A later
// signaling adapter must use this for each operation, including established WS.
func (s *Store) AuthorizeModule(ctx context.Context, token, hubID, moduleID string) (Principal, error) {
	p, err := s.Authenticate(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	if p.HubID != hubID || !contains(p.Modules, moduleID) {
		return Principal{}, ErrUnauthorized
	}
	return p, nil
}

// AdminRevoke is available only to the local operator. Revoking a Hub also
// invalidates all descendant installations and their outstanding pairings.
func (s *Store) AdminRevoke(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, "UPDATE identities SET revoked=1,version=version+1 WHERE (id=? OR hub_id=?) AND revoked=0", id, id)
	if err == nil {
		s.notifyRevocation()
	}
	return err
}

type AdminIdentity struct {
	ID      string `json:"id"`
	Tenant  string `json:"tenant"`
	Kind    string `json:"kind"`
	HubID   string `json:"hub_id"`
	Revoked bool   `json:"revoked"`
}

// AdminLookupFingerprint is offline-only recovery for an enrollment response
// lost before the local profile learned its server-assigned Hub ID.
func (s *Store) AdminLookupFingerprint(ctx context.Context, fingerprint string) ([]AdminIdentity, error) {
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != sha256.Size {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,tenant,kind,hub_id,revoked,public_key FROM identities ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AdminIdentity, 0)
	for rows.Next() {
		var item AdminIdentity
		var key []byte
		if err := rows.Scan(&item.ID, &item.Tenant, &item.Kind, &item.HubID, &item.Revoked, &key); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(key)
		if subtle.ConstantTimeCompare(sum[:], decoded) == 1 {
			result = append(result, item)
		}
	}
	return result, rows.Err()
}

func (s *Store) notifyRevocation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}

// AuthorizeContext provides a bounded authorization lifetime for future WS
// handlers. Same-process revocation wakes it immediately; another process's
// SQLite changes are checked at most one second later. Every command must
// still call AuthorizeModule at dispatch, rather than treating this as a lock.
func (s *Store) AuthorizeContext(parent context.Context, token, hubID, moduleID string) (context.Context, context.CancelFunc, error) {
	if _, err := s.AuthorizeModule(parent, token, hubID, moduleID); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			s.mu.Lock()
			changed := s.changed
			s.mu.Unlock()
			if _, err := s.AuthorizeModule(ctx, token, hubID, moduleID); err != nil {
				cancel(err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
			case <-ticker.C:
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled) }, nil
}

// Avoid accepting whitespace-normalized secrets: clients must preserve exact
// token bytes. This helper is for HTTP Authorization parsing only.
func Bearer(header string) string {
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if len(token) != 43 {
		return ""
	}
	return token
}
