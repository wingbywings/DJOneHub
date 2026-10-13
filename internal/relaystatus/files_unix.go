//go:build darwin || linux

package relaystatus

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const maxBytes = 128 << 10

type writer struct {
	path string
	lock *os.File
}

func private(info os.FileInfo, directory bool) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && info.Mode().Perm()&0077 == 0 &&
		((directory && info.IsDir()) || (!directory && info.Mode().IsRegular() && st.Nlink == 1))
}
func validDirectory(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Lstat(filepath.Dir(path))
	return err == nil && private(info, true)
}
func openPrivate(path string, flags int) (*os.File, error) {
	if !validDirectory(path) {
		return nil, ErrUnavailable
	}
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrUnavailable
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !private(info, false) || info.Size() > maxBytes {
		f.Close()
		return nil, ErrUnavailable
	}
	return f, nil
}
func newWriter(path string) (*writer, error) {
	f, err := openPrivate(path+".lock", syscall.O_RDWR|syscall.O_CREAT)
	if err != nil {
		return nil, err
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, ErrUnavailable
	}
	return &writer{path: path, lock: f}, nil
}
func (w *writer) close() { w.lock.Close() }
func (w *writer) write(snapshot Snapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > maxBytes || !validDirectory(w.path) {
		return ErrUnavailable
	}
	if info, err := os.Lstat(w.path); err == nil {
		if !private(info, false) {
			return ErrUnavailable
		}
	} else if !os.IsNotExist(err) {
		return ErrUnavailable
	}
	f, err := os.CreateTemp(filepath.Dir(w.path), ".runtime-status-*")
	if err != nil {
		return ErrUnavailable
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	if os.Rename(f.Name(), w.path) != nil {
		return ErrUnavailable
	}
	return nil
}
func Read(path string, now time.Time) Observation {
	f, err := openPrivate(path, syscall.O_RDONLY)
	if err != nil {
		return Observation{State: "unavailable"}
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, maxBytes+1))
	d.DisallowUnknownFields()
	var s Snapshot
	if d.Decode(&s) != nil || d.Decode(new(any)) != io.EOF || len(s.OnlineIdentities) > 128 {
		return Observation{State: "unavailable"}
	}
	return Observe(s, now)
}
