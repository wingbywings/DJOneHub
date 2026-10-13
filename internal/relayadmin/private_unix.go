//go:build darwin || linux

package relayadmin

import (
	"errors"
	"os"
	"syscall"
)

func identityError() error { return errors.New("invalid administration audit object") }
func prepareAuditFile(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
			return errors.New("unsafe private administration audit file")
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}
