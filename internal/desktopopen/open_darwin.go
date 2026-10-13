//go:build darwin && cgo

// Package desktopopen opens a launcher URL without putting its activation
// fragment in a subprocess argument, shell history or application log.
package desktopopen

/*
#cgo LDFLAGS: -framework AppKit -framework Foundation
#include <stdlib.h>
int dj_web_open(const char *url);
*/
import "C"
import (
	"errors"
	"unsafe"
)

func Open(link string) error {
	value := C.CString(link)
	defer C.free(unsafe.Pointer(value))
	if C.dj_web_open(value) != 1 {
		return errors.New("cannot open local management page")
	}
	return nil
}
