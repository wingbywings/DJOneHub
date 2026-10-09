//go:build darwin && cgo

package relayprofile

/*
#cgo LDFLAGS: -framework Security -framework Foundation -framework LocalAuthentication
#include <stdlib.h>
int dj_remote_vault(int operation, const char *account, const void *data, size_t length, void **output, size_t *output_length);
*/
import "C"

import (
	"fmt"
	"unsafe"
)

type keychainVault struct{}

func SystemVault() (Vault, error)                      { return keychainVault{}, nil }
func (keychainVault) Load(name string) ([]byte, error) { return keychainCall(0, name, nil) }
func (keychainVault) Create(name string, data []byte) error {
	_, err := keychainCall(1, name, data)
	return err
}
func (keychainVault) Replace(name string, data []byte) error {
	_, err := keychainCall(2, name, data)
	return err
}
func keychainCall(operation int, name string, data []byte) ([]byte, error) {
	if !validName(name) || len(data) > 32768 {
		return nil, ErrInvalid
	}
	account := C.CString(name)
	defer C.free(unsafe.Pointer(account))
	var input unsafe.Pointer
	if len(data) > 0 {
		input = unsafe.Pointer(&data[0])
	}
	var output unsafe.Pointer
	var length C.size_t
	status := C.dj_remote_vault(C.int(operation), account, input, C.size_t(len(data)), &output, &length)
	if output != nil {
		defer C.free(output)
	}
	switch status {
	case 0:
		if length > 32768 {
			return nil, ErrVault
		}
		return C.GoBytes(output, C.int(length)), nil
	case -25300:
		return nil, ErrNotFound
	case -25299:
		return nil, ErrExists
	default:
		return nil, fmt.Errorf("%w (OSStatus %d)", ErrVault, int(status))
	}
}
