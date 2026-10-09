//go:build !darwin && !linux

package turncred

func ReadConfig(string) (*Issuer, error) { return nil, ErrConfig }
