//go:build !darwin && !linux

package relayprofile

func lockProfile(string, string) (func(), error) { return nil, ErrUnsupported }
