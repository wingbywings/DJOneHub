//go:build !darwin || !cgo

package desktopopen

import "errors"

func QRPNG([]byte) ([]byte, error) { return nil, errors.New("QR encoding requires macOS") }
