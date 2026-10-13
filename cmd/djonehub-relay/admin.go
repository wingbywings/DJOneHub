package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayadmin"
	"github.com/iniwex5/vohive/internal/relaystatus"
	"github.com/iniwex5/vohive/internal/webadmin"
)

// mountAdministration keeps native Origin/Bearer guards intact: only the
// selected administration subtree reaches the browser session handler.
func mountAdministration(native *relay.Server, store *identity.Store, directory, origin, tenant, path string) (http.Handler, func(), error) {
	base, err := relayadmin.NormalizeBasePath(path)
	if err != nil || !identity.HTTPSOrigin(origin) || !filepath.IsAbs(directory) {
		return nil, nil, errors.New("integrated administration requires a private absolute directory, HTTPS origin, tenant and valid path")
	}
	auth, err := webadmin.Open(webadmin.Options{Directory: directory, Origin: origin, Tenant: tenant, CookieName: "djonehub_relay_admin", LoopbackOnly: true})
	if err != nil {
		return nil, nil, err
	}
	admin, err := relayadmin.New(relayadmin.Options{Auth: auth, Identity: store, Directory: directory, RelayOrigin: origin, BasePath: base, Build: "web-management-v3", RuntimeStatus: func() relaystatus.Observation {
		return relaystatus.Observe(native.RuntimeSnapshot(context.Background()), time.Now().UTC())
	}})
	if err != nil {
		auth.Close()
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(base+"/", admin)
	// Let the guarded admin ServeMux perform the missing-slash redirect.
	mux.Handle(base, admin)
	mux.Handle("/", native)
	var once sync.Once
	return mux, func() { once.Do(func() { admin.Close(); auth.Close() }) }, nil
}

func separateAdminDirectory(identityDir, adminDir string) bool {
	identityDir, adminDir = filepath.Clean(identityDir), filepath.Clean(adminDir)
	return identityDir != adminDir && !strings.HasPrefix(adminDir, identityDir+string(filepath.Separator)) && !strings.HasPrefix(identityDir, adminDir+string(filepath.Separator))
}
