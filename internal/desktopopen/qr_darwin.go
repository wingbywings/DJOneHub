//go:build darwin && cgo

package desktopopen

/*
#cgo LDFLAGS: -framework CoreImage
#include <stdlib.h>
int dj_web_qr(const void *input,size_t length,void **output,size_t *out_length);
*/
import "C"
import (
	"errors"
	"unsafe"
)

func QRPNG(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > 4096 {
		return nil, errors.New("pairing payload too large for QR")
	}
	var output unsafe.Pointer
	var length C.size_t
	if C.dj_web_qr(unsafe.Pointer(&data[0]), C.size_t(len(data)), &output, &length) != 1 {
		return nil, errors.New("pairing QR unavailable")
	}
	defer C.free(output)
	if length > 1<<20 {
		return nil, errors.New("invalid pairing QR")
	}
	return C.GoBytes(output, C.int(length)), nil
}
