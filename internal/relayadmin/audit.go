package relayadmin

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type auditStore struct{ db *sql.DB }
type auditEvent struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Username string    `json:"username"`
	Tenant   string    `json:"tenant"`
	Action   string    `json:"action"`
	ObjectID string    `json:"object_id"`
	Result   string    `json:"result"`
}

func openAudit(directory string) (*auditStore, error) {
	path := filepath.Join(directory, "audit.sqlite")
	if err := prepareAuditFile(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, username TEXT NOT NULL, tenant TEXT NOT NULL, action TEXT NOT NULL, object_id TEXT NOT NULL, result TEXT NOT NULL);"); err != nil {
		db.Close()
		return nil, err
	}
	return &auditStore{db}, nil
}
func (a *auditStore) begin(ctx context.Context, user, tenant, action, id string) (int64, error) {
	if len(id) > 128 {
		return 0, identityError()
	}
	result, err := a.db.ExecContext(ctx, "INSERT INTO events(at,username,tenant,action,object_id,result) VALUES(?,?,?,?,?,'pending')", time.Now().UnixMilli(), user, tenant, action, id)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
func (a *auditStore) finish(parent context.Context, id int64, result string) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	_, err := a.db.ExecContext(ctx, "UPDATE events SET result=? WHERE id=?", result, id)
	return err
}
func (a *auditStore) list(ctx context.Context) ([]auditEvent, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,at,username,tenant,action,object_id,result FROM events ORDER BY id DESC LIMIT 256")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []auditEvent{}
	for rows.Next() {
		var event auditEvent
		var at int64
		if err = rows.Scan(&event.ID, &at, &event.Username, &event.Tenant, &event.Action, &event.ObjectID, &event.Result); err != nil {
			return nil, err
		}
		event.At = time.UnixMilli(at).UTC()
		result = append(result, event)
	}
	return result, rows.Err()
}
