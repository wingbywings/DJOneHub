//go:build darwin || linux

package relayprofile

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/iniwex5/vohive/internal/callsignal"
)

func ReadPrivateJSON(path string, value any) error {
	if !filepath.IsAbs(path) {
		return ErrInvalid
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return ErrInvalid
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ErrInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 || int(stat.Uid) != os.Getuid() || info.Size() > 8192 {
		return ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 || callsignal.Decode(data, value) != nil {
		return ErrInvalid
	}
	return nil
}
func WritePrivateJSON(path string, value any) error {
	if !filepath.IsAbs(path) {
		return ErrInvalid
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return ErrInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() {
		return ErrInvalid
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrInvalid
	}
	ok = false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return ErrInvalid
	}
	if err := file.Sync(); err != nil {
		return ErrInvalid
	}
	if err := file.Close(); err != nil {
		return ErrInvalid
	}
	ok = true
	return nil
}
