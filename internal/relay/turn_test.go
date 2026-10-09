//go:build cgo

package relay

import (
	"context"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/turncred"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTURNRESTScopesRevocationAndTLSClient(t *testing.T) {
	issuer, err := turncred.New(turncred.Config{URLs: []string{"turn:relay.example:3478?transport=udp"}, Secret: strings.Repeat("s", 43), RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	f := signalSetup(t, WithTURN(issuer))
	body := map[string]string{"module_id": "module"}
	response := request(f.server, http.MethodPost, "/v1/ice", f.phone.AccessToken, body)
	c := readResponse[turncred.Credentials](t, response, http.StatusOK)
	if c.Validate(time.Now()) != nil || !c.Enabled || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invalid TURN response")
	}
	if request(f.server, http.MethodPost, "/v1/ice", f.phone.AccessToken, map[string]string{"module_id": "other"}).Code != http.StatusUnauthorized {
		t.Fatal("installation exceeded module scope")
	}
	if request(f.server, http.MethodPost, "/v1/ice", "", body).Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated TURN credential")
	}
	api, err := relayclient.New(f.http.URL, relayclient.Options{TLSConfig: f.tls})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if _, err := api.ICE(context.Background(), f.hub.AccessToken, "module"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeInstallation(context.Background(), f.hub.AccessToken, f.phone.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if request(f.server, http.MethodPost, "/v1/ice", f.phone.AccessToken, body).Code != http.StatusUnauthorized {
		t.Fatal("revoked installation obtained TURN")
	}
}
func TestTURNDisabledRemainsExplicitDirectMode(t *testing.T) {
	f := signalSetup(t)
	c := readResponse[turncred.Credentials](t, request(f.server, http.MethodPost, "/v1/ice", f.hub.AccessToken, map[string]string{"module_id": "module"}), http.StatusOK)
	if c.Enabled || len(c.Servers) != 0 || c.Validate(time.Now()) != nil {
		t.Fatal(c)
	}
}
