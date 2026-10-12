package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/remotecall"
)

// One bounded record per module. No number, token, SDP or captured audio is
// stored. A modem index cannot prove call continuity across a process restart.
type remoteJournalRecord struct {
	CallUUID   string    `json:"call_uuid"`
	Generation uint64    `json:"generation"`
	Phase      string    `json:"phase"`
	UpdatedAt  time.Time `json:"updated_at"`
}
type remoteJournalFile struct {
	Version int                  `json:"version"`
	Record  *remoteJournalRecord `json:"record,omitempty"`
}
type remoteJournalStatus struct {
	RecoveryRequired  bool   `json:"recovery_required"`
	PersistenceFailed bool   `json:"persistence_failed"`
	Phase             string `json:"phase,omitempty"`
}
type remoteJournal struct {
	mu               sync.Mutex
	path             string
	record           *remoteJournalRecord
	recovery, failed bool
	startupObserved  bool
	missingPath      bool
}

func openRemoteJournal(path string) *remoteJournal {
	j := &remoteJournal{path: path}
	if path == "" {
		return j
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return j
	}
	if err != nil {
		j.recovery, j.failed = true, true
		return j
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	var stored remoteJournalFile
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err != nil || len(data) > 4096 || d.Decode(&stored) != nil || d.Decode(new(any)) != io.EOF || stored.Version != 1 {
		j.recovery, j.failed = true, true
		return j
	}
	if r := stored.Record; r != nil {
		if r.CallUUID == "" || len(r.CallUUID) > 128 || r.Generation == 0 || r.UpdatedAt.IsZero() || !journalPhase(r.Phase) {
			j.recovery, j.failed = true, true
			return j
		}
		j.record = r
		j.recovery = r.Phase != "ended"
	}
	return j
}
func journalPhase(p string) bool {
	switch p {
	case "offered", "answer_pending", "active", "ending", "ended", "unverified":
		return true
	}
	return false
}

// The first successful snapshot may reveal an already active call even when
// no unfinished record survived. It still cannot be auto-attached to a phone.
func (j *remoteJournal) observeStartup(calls []parsedCall) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.startupObserved {
		return
	}
	j.startupObserved = true
	if j.recovery || (j.record != nil && j.record.Phase != "ended") {
		return
	}
	for _, call := range calls {
		if call.State == "active" || call.State == "held" || call.State == "dialing" || call.State == "alerting" {
			j.recovery = true
			_ = j.saveLocked(&remoteJournalRecord{CallUUID: callcontrol.NewCallUUID(), Generation: 1, Phase: "unverified", UpdatedAt: time.Now()})
			return
		}
	}
}
func (j *remoteJournal) status() remoteJournalStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := remoteJournalStatus{RecoveryRequired: j.recovery || j.failed, PersistenceFailed: j.failed}
	if j.record != nil {
		s.Phase = j.record.Phase
	}
	return s
}
func (j *remoteJournal) begin(b remotecall.Binding) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.recovery || j.failed {
		return errors.New("remote call journal requires local reconciliation")
	}
	if b.CallUUID == "" || b.Generation == 0 {
		return remotecall.ErrBinding
	}
	if j.record != nil && j.record.CallUUID == b.CallUUID && j.record.Generation == b.Generation {
		if j.record.Phase == "ended" {
			return remotecall.ErrState
		}
		return nil
	}
	return j.saveLocked(&remoteJournalRecord{CallUUID: b.CallUUID, Generation: b.Generation, Phase: "offered", UpdatedAt: time.Now()})
}
func (j *remoteJournal) phase(b remotecall.Binding, phase string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !journalPhase(phase) || j.record == nil || j.record.CallUUID != b.CallUUID || j.record.Generation != b.Generation || j.recovery {
		return remotecall.ErrBinding
	}
	if j.record.Phase == phase {
		if j.failed {
			return errors.New("remote call journal write failed")
		}
		return nil
	}
	if j.record.Phase == "ended" {
		return remotecall.ErrState
	}
	r := *j.record
	r.Phase = phase
	r.UpdatedAt = time.Now()
	return j.saveLocked(&r)
}

// Only a complete, successful CLCC snapshot is evidence that the module is
// idle. USB loss, synthetic teardown and an AT error must never clear recovery.
func (j *remoteJournal) uncertain() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.record != nil && j.record.Phase != "ended" {
		j.recovery = true
	}
}
func (j *remoteJournal) confirmedIdle() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.recovery && !j.failed && (j.record == nil || j.record.Phase == "ended") {
		return nil
	}
	var next *remoteJournalRecord
	if j.record != nil {
		r := *j.record
		r.Phase = "ended"
		r.UpdatedAt = time.Now()
		next = &r
	}
	if err := j.saveLocked(next); err != nil {
		return err
	}
	j.recovery = false
	return nil
}
func (j *remoteJournal) saveLocked(record *remoteJournalRecord) error {
	if j.missingPath {
		j.failed = true
		return errors.New("remote journal storage unavailable")
	}
	err := writeRemoteJournal(j.path, remoteJournalFile{Version: 1, Record: record})
	j.failed = err != nil
	if err == nil {
		j.record = record
	}
	return err
}
func writeRemoteJournal(path string, stored remoteJournalFile) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".remote-call-journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (a *app) ensureRemoteJournal() *remoteJournal {
	a.remoteJournalOnce.Do(func() {
		path := a.remoteJournalPath
		if path == "" && !a.demo {
			settings := a.remoteCallSettingsPath
			if settings == "" {
				if dir, err := os.UserConfigDir(); err == nil {
					settings = filepath.Join(dir, "DJOneHub", "remote-calls.json")
				}
			}
			if settings != "" {
				path = filepath.Join(filepath.Dir(settings), "remote-call-journal.json")
			}
		}
		a.remoteJournal = openRemoteJournal(path)
		if path == "" && !a.demo {
			a.remoteJournal.missingPath = true
			a.remoteJournal.failed = true
		}
	})
	return a.remoteJournal
}
