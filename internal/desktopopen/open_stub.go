//go:build !darwin || !cgo

package desktopopen

import "errors"

func Open(string) error { return errors.New("native page opening requires macOS") }
