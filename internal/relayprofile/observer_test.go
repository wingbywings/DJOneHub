//go:build cgo && (darwin || linux)

package relayprofile

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type observerVault struct {
	*memoryVault
	denied atomic.Bool
}

func (v *observerVault) Load(id string) ([]byte, error) {
	if v.denied.Load() {
		return nil, ErrVault
	}
	return v.memoryVault.Load(id)
}

func TestInstallationObservationNeverReopensVaultAndStopsAtExpiry(t *testing.T) {
	f := setup(t, nil)
	f.enroll(t)
	v := &observerVault{memoryVault: f.vault}
	s := New(v, f.store.directory, f.options)
	o, err := s.InstallationObserver(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	v.denied.Store(true)
	for i := 0; i < 3; i++ {
		if _, err := o.Read(context.Background()); err != nil {
			t.Fatal("observer reopened vault", err)
		}
	}
	o.expires = time.Now().Add(-time.Second)
	if _, err := o.Read(context.Background()); err == nil {
		t.Fatal("expired observer refreshed its credential")
	}
}
