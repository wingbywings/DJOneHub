//go:build !darwin && !linux

package relayprofile

func ReadPrivateJSON(string, any) error  { return ErrUnsupported }
func WritePrivateJSON(string, any) error { return ErrUnsupported }
