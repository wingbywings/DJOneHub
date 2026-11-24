package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/iniwex5/vohive/internal/smsout"
)

// Only queued work may be claimed. The durable sending transition is never
// rolled back or retried, even after a process crash or a lost HTTP response.
func expireSMS(ctx context.Context, tx *sql.Tx, now time.Time) error {
	for _, q := range []struct {
		sql    string
		before int64
	}{
		{`DELETE FROM sms_outgoing WHERE created<?`, now.Add(-7 * 24 * time.Hour).Unix()},
		{`UPDATE sms_outgoing SET state='failed',code='request_expired',phone='',message='' WHERE state='queued' AND created<?`, now.Add(-2 * time.Minute).Unix()},
		{`UPDATE sms_outgoing SET state='unknown',code='submission_uncertain',phone='',message='' WHERE state='sending' AND updated<?`, now.Add(-30 * time.Minute).Unix()},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.before); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SubmitSMS(ctx context.Context, access string, request smsout.Request) (smsout.Result, error) {
	var result smsout.Result
	if request.Validate() != "" {
		return result, ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	p, err := s.authenticate(ctx, tx, access)
	if err != nil || p.Kind != "installation" || !contains(p.Modules, request.ModuleID) {
		return result, ErrUnauthorized
	}
	request.InstallationID = p.ID
	// Normalize time before hashing so equivalent RFC3339 representations match.
	request.CreatedAt = request.CreatedAt.UTC()
	if err = expireSMS(ctx, tx, s.now()); err != nil {
		return result, err
	}
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT digest,state,segments,code FROM sms_outgoing WHERE identity_id=? AND id=?`, p.ID, request.ID).Scan(&digest, &result.State, &result.Segments, &result.Code)
	result.ID, result.InstallationID = request.ID, p.ID
	if err == nil {
		if digest != request.Digest() {
			return result, ErrInvalid
		}
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if request.CreatedAt.Before(s.now().Add(-2*time.Minute)) || request.CreatedAt.After(s.now().Add(time.Minute)) {
		result.State, result.Code = "failed", "request_expired"
		return result, tx.Commit()
	}
	var total, pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(state IN ('queued','sending')),0) FROM sms_outgoing WHERE hub_id=?`, p.HubID).Scan(&total, &pending); err != nil {
		return result, err
	}
	if total >= 4096 || pending >= 20 {
		return result, ErrCapacity
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sms_outgoing(identity_id,id,hub_id,module_id,digest,phone,message,created,request_time,state,segments,code,updated) VALUES(?,?,?,?,?,?,?,?,?,'queued',0,'',?)`,
		p.ID, request.ID, p.HubID, request.ModuleID, request.Digest(), request.Phone, request.Message, request.CreatedAt.Unix(), request.CreatedAt.Format(time.RFC3339Nano), s.now().Unix())
	if err != nil {
		return result, err
	}
	result.State = "queued"
	return result, tx.Commit()
}

func (s *Store) ClaimSMS(ctx context.Context, access string) (*smsout.Request, error) {
	tx, err := s.transaction(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	hub, err := s.authenticate(ctx, tx, access)
	if err != nil || hub.Kind != "hub" {
		return nil, ErrUnauthorized
	}
	if err = expireSMS(ctx, tx, s.now()); err != nil {
		return nil, err
	}
	var request smsout.Request
	var timestamp string
	err = tx.QueryRowContext(ctx, `SELECT id,identity_id,module_id,phone,message,request_time FROM sms_outgoing WHERE hub_id=? AND state='queued' ORDER BY created,id LIMIT 1`, hub.ID).
		Scan(&request.ID, &request.InstallationID, &request.ModuleID, &request.Phone, &request.Message, &timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	request.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return nil, err
	}
	var modules string
	err = tx.QueryRowContext(ctx, `SELECT modules FROM identities WHERE id=? AND hub_id=? AND tenant=? AND kind='installation' AND revoked=0 AND refresh_expires>?`, request.InstallationID, hub.ID, hub.Tenant, s.now().Unix()).Scan(&modules)
	var allowed []string
	if err != nil || json.Unmarshal([]byte(modules), &allowed) != nil || !contains(allowed, request.ModuleID) || !contains(hub.Modules, request.ModuleID) {
		_, err = tx.ExecContext(ctx, `UPDATE sms_outgoing SET state='failed',code='unauthorized',phone='',message='' WHERE identity_id=? AND id=?`, request.InstallationID, request.ID)
		if err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE sms_outgoing SET state='sending',updated=? WHERE identity_id=? AND id=?`, s.now().Unix(), request.InstallationID, request.ID)
	if err != nil {
		return nil, err
	}
	return &request, tx.Commit()
}

func (s *Store) FinishSMS(ctx context.Context, access string, result smsout.Result) error {
	if !result.Valid() {
		return ErrInvalid
	}
	tx, err := s.transaction(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hub, err := s.authenticate(ctx, tx, access)
	if err != nil || hub.Kind != "hub" {
		return ErrUnauthorized
	}
	var module, state, code string
	var segments int
	err = tx.QueryRowContext(ctx, `SELECT module_id,state,segments,code FROM sms_outgoing WHERE hub_id=? AND identity_id=? AND id=?`, hub.ID, result.InstallationID, result.ID).Scan(&module, &state, &segments, &code)
	if err != nil || !contains(hub.Modules, module) {
		return ErrUnauthorized
	}
	if state != "sending" && state != "unknown" {
		if state == result.State && segments == result.Segments && code == result.Code {
			return nil
		}
		return ErrInvalid
	}
	_, err = tx.ExecContext(ctx, `UPDATE sms_outgoing SET state=?,segments=?,code=?,phone='',message='',updated=? WHERE identity_id=? AND id=?`, result.State, result.Segments, result.Code, s.now().Unix(), result.InstallationID, result.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
