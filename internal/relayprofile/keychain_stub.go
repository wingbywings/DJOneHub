//go:build !darwin || !cgo

package relayprofile

func SystemVault() (Vault, error) { return nil, ErrUnsupported }
