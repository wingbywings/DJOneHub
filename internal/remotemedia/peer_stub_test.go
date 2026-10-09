//go:build !cgo || !remoteopus

package remotemedia

import (
	"context"
	"errors"
	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"testing"
)

func TestPeerRequiresActualOpusBuild(t *testing.T) {
	p, err := NewPeer(context.Background(), PeerConfig{CallUUID: "call", Generation: 1})
	if p != nil || !errors.Is(err, opus.ErrUnavailable) {
		t.Fatalf("peer=%v err=%v", p, err)
	}
}
