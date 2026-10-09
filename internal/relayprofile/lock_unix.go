//go:build darwin || linux

package relayprofile

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func lockProfile(directory, name string) (func(), error) {
	if !filepath.IsAbs(directory) {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, ErrInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() {
		return nil, ErrInvalid
	}
	path := filepath.Join(directory, name+".lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrInvalid
	}
	file := os.NewFile(uintptr(fd), path)
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, ErrInvalid
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		file.Close()
		return nil, ErrInvalid
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrBusy
	}
	var once sync.Once
	return func() { once.Do(func() { syscall.Flock(fd, syscall.LOCK_UN); file.Close() }) }, nil
}
