//go:build !cgo || !remoteopus

package opus

import (
	"errors"
	"testing"
)

func TestBuildWithoutOpusFailsClosed(t *testing.T) {
	if Available() {
		t.Fatal("codec advertised")
	}
	if _, err := New(DefaultConfig()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
