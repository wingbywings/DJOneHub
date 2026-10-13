//go:build darwin || linux

package webadmin

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockDirectory(directory string) (func(), error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, ErrConfig
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) {
		return nil, ErrConfig
	}
	fd, err := syscall.Open(filepath.Join(directory, "admin.lock"), syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrConfig
	}
	file := os.NewFile(uintptr(fd), "admin.lock")
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, ErrConfig
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		file.Close()
		return nil, ErrConfig
	}
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		file.Close()
		return nil, ErrConfig
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}
