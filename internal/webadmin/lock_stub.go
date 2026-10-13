//go:build !darwin && !linux

package webadmin

func lockDirectory(string) (func(), error) { return nil, ErrConfig }
