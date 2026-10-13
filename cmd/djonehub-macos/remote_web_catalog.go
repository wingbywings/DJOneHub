package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayprofile"
	_ "github.com/mattn/go-sqlite3"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type remoteProfileEntry struct {
	Profile   string                      `json:"profile"`
	Label     string                      `json:"label"`
	Status    relayprofile.Status         `json:"status"`
	Request   *identity.EnrollmentRequest `json:"request,omitempty"`
	Error     string                      `json:"error,omitempty"`
	CheckedAt time.Time                   `json:"checked_at"`
	AdminURL  string                      `json:"admin_url,omitempty"`
}
type remoteCatalog struct{ db *sql.DB }

func openRemoteCatalog(directory string) (*remoteCatalog, error) {
	path := filepath.Join(directory, "profiles.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
			return nil, relayprofile.ErrInvalid
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	syscall.Close(fd)
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > 1 {
		db.Close()
		return nil, relayprofile.ErrInvalid
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS profiles(id TEXT PRIMARY KEY, entry TEXT NOT NULL); CREATE TABLE IF NOT EXISTS settings(id INTEGER PRIMARY KEY CHECK(id=1), autostart TEXT NOT NULL); INSERT OR IGNORE INTO settings VALUES(1,'');`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("PRAGMA user_version=1"); err != nil {
		db.Close()
		return nil, err
	}
	return &remoteCatalog{db}, nil
}
func (c *remoteCatalog) list(ctx context.Context) ([]remoteProfileEntry, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT entry FROM profiles ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []remoteProfileEntry{}
	for rows.Next() {
		var data string
		var entry remoteProfileEntry
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(data), &entry) != nil {
			return nil, relayprofile.ErrInvalid
		}
		items = append(items, entry)
	}
	return items, rows.Err()
}
func (c *remoteCatalog) get(ctx context.Context, id string) (remoteProfileEntry, error) {
	var data string
	var entry remoteProfileEntry
	err := c.db.QueryRowContext(ctx, "SELECT entry FROM profiles WHERE id=?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return entry, relayprofile.ErrNotFound
	}
	if err != nil {
		return entry, err
	}
	if json.Unmarshal([]byte(data), &entry) != nil || entry.Profile != id {
		return entry, relayprofile.ErrInvalid
	}
	return entry, nil
}
func (c *remoteCatalog) save(ctx context.Context, entry remoteProfileEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM profiles WHERE id!=?", entry.Profile).Scan(&count)
	if err != nil {
		return err
	}
	if count >= 32 {
		return relayprofile.ErrBusy
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO profiles(id,entry) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET entry=excluded.entry", entry.Profile, string(data)); err != nil {
		return err
	}
	return tx.Commit()
}
func (c *remoteCatalog) remove(ctx context.Context, id string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE settings SET autostart='' WHERE autostart=?", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM profiles WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
func (c *remoteCatalog) autostart(ctx context.Context) (string, error) {
	var id string
	err := c.db.QueryRowContext(ctx, "SELECT autostart FROM settings WHERE id=1").Scan(&id)
	return id, err
}
func (c *remoteCatalog) setAutostart(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, "UPDATE settings SET autostart=? WHERE id=1", id)
	return err
}
