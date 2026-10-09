//go:build darwin || linux

package turncred

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func ReadConfig(path string) (*Issuer, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrConfig
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrConfig
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, ErrConfig
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || info.Size() > 8192 {
		return nil, ErrConfig
	}
	var config Config
	d := json.NewDecoder(io.LimitReader(f, 8193))
	d.DisallowUnknownFields()
	if d.Decode(&config) != nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrConfig
	}
	return New(config)
}
