//go:build !darwin && !linux

package voippush

func ReadConfig(string) (*Client, error) { return nil, ErrConfig }
