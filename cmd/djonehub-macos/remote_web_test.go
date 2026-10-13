package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayadmin"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/relayprofile"
	"github.com/iniwex5/vohive/internal/relaystatus"
	"github.com/iniwex5/vohive/internal/remotehub"
	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"github.com/iniwex5/vohive/internal/webadmin"
)

type webTestVault struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (v *webTestVault) Load(id string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, ok := v.data[id]
	if !ok {
		return nil, relayprofile.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}
func (v *webTestVault) Create(id string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.data[id]; ok {
		return relayprofile.ErrExists
	}
	v.data[id] = append([]byte(nil), data...)
	return nil
}
func (v *webTestVault) Replace(id string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.data[id]; !ok {
		return relayprofile.ErrNotFound
	}
	v.data[id] = append([]byte(nil), data...)
	return nil
}
func (v *webTestVault) Delete(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.data, id)
	return nil
}

type webFixture struct {
	t                    *testing.T
	m                    *remoteWebManager
	hub                  *usbDeviceHub
	store                *relayprofile.Store
	relay                *identity.Store
	relayService         *relay.Server
	origin, relayOrigin  string
	handler              http.Handler
	cookie               *http.Cookie
	csrf                 string
	factoryCalls         atomic.Int32
	signalingConnections atomic.Int32
}

func newWebFixture(t *testing.T) *webFixture {
	t.Helper()
	return newWebFixtureAt(t, "http://127.0.0.1:51575")
}
func newWebFixtureAt(t *testing.T, origin string) *webFixture {
	t.Helper()
	f := &webFixture{t: t, origin: origin}
	state, err := identity.Open(filepath.Join(t.TempDir(), "relay"))
	if err != nil {
		t.Fatal(err)
	}
	f.relay = state
	native := httptest.NewUnstartedServer(nil)
	f.relayOrigin = "https://" + native.Listener.Addr().String()
	service := relay.NewServer(state, relay.WithEnrollmentOrigin(f.relayOrigin), relay.WithBuild("web-display-browser-fixture"))
	f.relayService = service
	native.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/signaling" {
			f.signalingConnections.Add(1)
		}
		service.ServeHTTP(w, r)
	})
	native.StartTLS()
	pool := x509.NewCertPool()
	pool.AddCert(native.Certificate())
	f.store = relayprofile.New(&webTestVault{data: map[string][]byte{}}, filepath.Join(t.TempDir(), "locks"), relayclient.Options{TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	f.hub = newUSBDeviceHub()
	a, b := remoteTestApp(t), remoteTestApp(t)
	a.deviceID = "module_a"
	b.deviceID = "module_b"
	f.hub.devices["module_a"] = &managedUSBDevice{ID: "module_a", Alias: "同名模块", State: "ready", Runtime: a}
	f.hub.devices["module_b"] = &managedUSBDevice{ID: "module_b", Alias: "同名模块", State: "ready", Runtime: b}
	m, err := newRemoteWebManager(context.Background(), f.hub, filepath.Join(t.TempDir(), "management"), f.origin, "fixture-native-control", func() (*relayprofile.Store, error) { f.factoryCalls.Add(1); return f.store, nil })
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	f.hub.remoteWeb = m
	m.connect = func(ctx context.Context, id string, _ *relayprofile.Store) (*remoteHubService, error) {
		manager := remotehub.Start(ctx, remotehub.Config{Connect: func(ctx context.Context) (remotehub.Connection, error) { return newWebConnection(), nil }, Refresh: func(context.Context) (string, error) { return "fixture", nil }})
		return &remoteHubService{Manager: manager}, nil
	}
	f.handler = f.hub.routes(webAssets)
	response := f.request("POST", remoteWebAPI+"/session/local", map[string]string{}, false)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	f.cookie = response.Result().Cookies()[0]
	var login struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(response.Body.Bytes(), &login)
	f.csrf = login.CSRF
	t.Cleanup(func() { m.Close(); f.hub.close(); native.Close(); service.Close(); state.Close() })
	return f
}
func (f *webFixture) request(method, path string, body any, auth bool) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, f.origin+path, bytes.NewReader(data))
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		r.Header.Set("Origin", f.origin)
	}
	if auth {
		r.AddCookie(f.cookie)
		r.Header.Set("X-DJOneHub-CSRF", f.csrf)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func (f *webFixture) work(path string, body any) remoteWebJob {
	f.t.Helper()
	w := f.request("POST", remoteWebAPI+path, body, true)
	if w.Code != 202 {
		f.t.Fatalf("job start %d: %s", w.Code, w.Body.String())
	}
	var j remoteWebJob
	json.Unmarshal(w.Body.Bytes(), &j)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := f.m.jobState(j.ID)
		if !ok {
			f.t.Fatal("missing job")
		}
		if j.State != "running" {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	f.t.Fatal("job timeout")
	return j
}
func (f *webFixture) ready(id string, modules []string) {
	f.t.Helper()
	g, err := f.relay.ProvisionEnrollment(context.Background(), "tenant", modules)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.store.Enroll(context.Background(), id, f.relayOrigin, g); err != nil {
		f.t.Fatal(err)
	}
	j := f.work("/profiles/import", map[string]string{"profile": id, "name": id})
	if j.State != "completed" {
		f.t.Fatal(j.Error)
	}
}
func (f *webFixture) phone(id string, modules []string) (identity.Tokens, identity.Grant) {
	f.t.Helper()
	g, err := f.store.Pairing(context.Background(), id, modules)
	if err != nil {
		f.t.Fatal(err)
	}
	pub, key, _ := ed25519.GenerateKey(nil)
	proof := identity.GrantProof{ID: g.ID, Secret: g.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("pairing", proof))
	tokens, err := f.relay.Redeem(context.Background(), proof)
	if err != nil {
		f.t.Fatal(err)
	}
	return tokens, g
}
func webResult[T any](t *testing.T, j remoteWebJob) T {
	t.Helper()
	if j.State != "completed" {
		t.Fatalf("job %s: %s", j.Kind, j.Error)
	}
	data, _ := json.Marshal(j.Result)
	var result T
	if json.Unmarshal(data, &result) != nil {
		t.Fatal("invalid result")
	}
	return result
}

type webConnection struct {
	done chan struct{}
	once sync.Once
}

func newWebConnection() *webConnection                       { return &webConnection{done: make(chan struct{})} }
func (c *webConnection) Done() <-chan struct{}               { return c.done }
func (c *webConnection) Messages() <-chan callsignal.Message { return nil }
func (c *webConnection) Close()                              { c.once.Do(func() { close(c.done) }) }
func (c *webConnection) MaintainAuthorization(ctx context.Context, _ func(context.Context) (string, error)) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRemoteWebSessionBoundaryAndReadDoesNotLoadVault(t *testing.T) {
	f := newWebFixture(t)
	if w := f.request("GET", remoteWebAPI+"/profiles", nil, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := f.request("GET", remoteWebAPI+"/profiles", nil, true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if f.factoryCalls.Load() != 0 {
		t.Fatal("GET touched profile vault")
	}
	r := httptest.NewRequest("POST", f.origin+remoteWebAPI+"/enrollment-requests", strings.NewReader(`{"name":"Mac","relay_url":"https://example.test","modules":["module_a"]}`))
	r.RemoteAddr = "127.0.0.1:4321"
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", f.origin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF missing accepted", w.Code)
	}
	r.Header.Set("X-DJOneHub-CSRF", f.csrf)
	r.Header.Set("Origin", "https://attacker.test")
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin accepted", w.Code)
	}
	r.Header.Set("Origin", f.origin)
	r.Host = "attacker.test"
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("Host accepted", w.Code)
	}
	if f.factoryCalls.Load() != 0 {
		t.Fatal("denied request loaded vault")
	}
	if w = f.request("PUT", "/api/devices/module_a/remote-calls/config", callcontrol.RemoteConfig{Enabled: true, TargetInstallationID: "forged"}, false); w.Code != 401 {
		t.Fatal("legacy route bypass", w.Code)
	}
	if w = f.request("PUT", "/api/remote-calls/config", callcontrol.RemoteConfig{}, true); w.Code != 400 {
		t.Fatal("default module route accepted", w.Code)
	}
}
func TestRemoteWebRegistrationBoundFileAndModuleChoices(t *testing.T) {
	f := newWebFixture(t)
	bad := f.work("/enrollment-requests", map[string]any{"name": "Mac", "relay_url": f.relayOrigin, "modules": []string{"unknown"}})
	if bad.Error != "scope_denied" {
		t.Fatal(bad.Error)
	}
	j := f.work("/enrollment-requests", map[string]any{"name": "Mac", "relay_url": f.relayOrigin, "modules": []string{"module_b"}})
	entry := webResult[remoteProfileEntry](t, j)
	if entry.Request == nil || len(entry.Request.Modules) != 1 || entry.Request.Modules[0].ID != "module_b" || entry.Request.Modules[0].Alias != "同名模块" {
		t.Fatal("incorrect module selection")
	}
	if w := f.request("GET", remoteWebAPI+"/profiles/"+entry.Profile+"/request", nil, true); w.Code != 200 || w.Header().Get("Content-Disposition") == "" {
		t.Fatal("request download")
	}
	submitted := f.work("/profiles/"+entry.Profile+"/actions", map[string]string{"action": "submit"})
	webResult[identity.EnrollmentRequestStatus](t, submitted)
	if err := f.relay.AdminApproveEnrollment(context.Background(), "tenant", entry.Request.ID, []string{"module_b"}); err != nil {
		t.Fatal(err)
	}
	file, err := f.relay.AdminIssueEnrollment(context.Background(), "tenant", entry.Request.ID, f.relayOrigin)
	if err != nil {
		t.Fatal(err)
	}
	wrong := file
	wrong.PublicKeyFingerprint = "wrong"
	failed := f.work("/profiles/"+entry.Profile+"/actions", map[string]any{"action": "enroll", "authorization": wrong})
	if failed.Error != "invalid_management_state" {
		t.Fatal(failed.Error)
	}
	valid := f.work("/profiles/"+entry.Profile+"/actions", map[string]any{"action": "enroll", "authorization": file})
	ready := webResult[remoteProfileEntry](t, valid)
	if ready.Status.Phase != "ready" || len(ready.Status.Principal.Modules) != 1 || ready.Status.Principal.Modules[0] != "module_b" {
		t.Fatal("registration scope")
	}
	var stored string
	if err = f.m.catalog.db.QueryRow("SELECT entry FROM profiles WHERE id=?", entry.Profile).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, file.Grant.Secret) || strings.Contains(stored, "refresh_token") || strings.Contains(stored, "access_token") || strings.Contains(stored, "\"seed\"") {
		t.Fatal("secret in profile catalog")
	}
	retry := f.work("/profiles/"+entry.Profile+"/actions", map[string]any{"action": "enroll", "authorization": file})
	if retry.Error != "credential_recovery_required" {
		t.Fatal("enrollment replay", retry.Error)
	}
}
func TestRemoteWebInvitationResultScopeRevocationAndRoute(t *testing.T) {
	f := newWebFixture(t)
	f.ready("owner", []string{"module_a", "module_b"})
	f.ready("other", []string{"module_a"})
	f.m.setActive("owner")
	first := f.work("/pairings", map[string]any{"profile": "owner", "name": "手机 A", "modules": []string{"module_a"}})
	delivery := webResult[pairingDelivery](t, first)
	pub, key, _ := ed25519.GenerateKey(nil)
	proof := identity.GrantProof{ID: delivery.Grant.ID, Secret: delivery.Grant.Secret, PublicKey: pub}
	proof.Signature = ed25519.Sign(key, identity.GrantMessage("pairing", proof))
	phone, err := f.relay.Redeem(context.Background(), proof)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.phone("owner", []string{"module_b"}) // unrelated newer identity must not become this invitation's result
	result := f.work("/pairings/"+delivery.Grant.ID+"/actions", map[string]string{"profile": "owner", "action": "status"})
	state := webResult[identity.GrantStatus](t, result)
	if state.IdentityID != phone.Principal.ID || state.State != "consumed" {
		t.Fatal("wrong invitation identity")
	}
	erased, _ := f.m.jobState(first.ID)
	if secret, ok := erased.Result.(pairingDelivery); !ok || secret.Grant.Secret != "" || secret.QRImage != "" {
		t.Fatal("completed pairing retained delivery secret")
	}
	other, _ := f.phone("other", []string{"module_a"})
	config := callcontrol.RemoteConfig{Enabled: true, TargetInstallationID: other.Principal.ID}
	invalid := f.work("/routes", map[string]any{"module": "module_a", "config": config})
	if invalid.Error != "scope_denied" {
		t.Fatal("cross-Hub target", invalid.Error)
	}
	config.TargetInstallationID = phone.Principal.ID
	a := f.hub.devices["module_a"].Runtime
	a.ensureVoiceAgent().mu.Lock()
	a.ensureVoiceAgent().state.Enabled = true
	a.ensureVoiceAgent().mu.Unlock()
	saved := f.work("/routes", map[string]any{"module": "module_a", "config": config})
	webResult[map[string]any](t, saved)
	actual, _ := a.ensureRemoteCalls().snapshot()
	if actual.TargetInstallationID != phone.Principal.ID || callcontrol.SelectRoute(a.ensureVoiceAgent().snapshot().Enabled, 0, actual).Mode != callcontrol.Agent {
		t.Fatal("AI priority changed")
	}
	rejected := f.work("/routes", map[string]any{"module": "module_b", "config": config})
	if rejected.Error != "scope_denied" {
		t.Fatal("module scope", rejected.Error)
	}
	revoked := f.work("/installations", map[string]string{"profile": "owner", "action": "revoke", "id": phone.Principal.ID})
	webResult[map[string]any](t, revoked)
	rejected = f.work("/routes", map[string]any{"module": "module_a", "config": config})
	if rejected.Error != "scope_denied" {
		t.Fatal("revoked target", rejected.Error)
	}
	actual, _ = a.ensureRemoteCalls().snapshot()
	if actual.TargetInstallationID != phone.Principal.ID {
		t.Fatal("revocation silently retargeted")
	}
}
func TestRemoteWebConnectionCallAndRecoveryGates(t *testing.T) {
	f := newWebFixture(t)
	f.ready("first", []string{"module_a"})
	f.ready("second", []string{"module_a"})
	connected := f.work("/connection", map[string]any{"profile": "first", "auto_start": true})
	webResult[map[string]any](t, connected)
	if f.m.activeProfile() != "first" {
		t.Fatal("connect selection")
	}
	a := f.hub.devices["module_a"].Runtime
	a.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "fixture"}}, time.Now())
	blocked := f.work("/connection", map[string]any{"profile": "second", "auto_start": true})
	if blocked.Error != "call_or_recovery_active" || f.m.activeProfile() != "first" {
		t.Fatal("call gate", blocked.Error)
	}
	a.callMu.Lock()
	a.activeCall = nil
	a.callMu.Unlock()
	journal := a.ensureRemoteJournal()
	journal.mu.Lock()
	journal.recovery = true
	journal.mu.Unlock()
	blocked = f.work("/connection", map[string]any{"profile": "second", "auto_start": true})
	if blocked.Error != "call_or_recovery_active" {
		t.Fatal("recovery gate", blocked.Error)
	}
	journal.mu.Lock()
	journal.recovery = false
	journal.mu.Unlock()
	changed := f.work("/connection", map[string]any{"profile": "second", "auto_start": false})
	webResult[map[string]any](t, changed)
	if f.m.activeProfile() != "second" {
		t.Fatal("switch selection")
	}
	startup, _ := f.m.catalog.autostart(context.Background())
	if startup != "" {
		t.Fatal("unchecked startup kept old selection")
	}
	removed := f.work("/profiles/second/actions", map[string]string{"action": "remove"})
	if removed.Error != "call_or_recovery_active" {
		t.Fatal("active profile removal")
	}
	disconnected := f.work("/connection", map[string]any{"profile": "", "auto_start": true})
	webResult[map[string]any](t, disconnected)
	removed = f.work("/profiles/second/actions", map[string]string{"action": "remove"})
	webResult[map[string]any](t, removed)
	if _, err := f.store.Status("second"); !errors.Is(err, relayprofile.ErrNotFound) {
		t.Fatal("local identity not removed")
	}
}
func TestRemoteWebBusyJobDoesNotBlockReadsOrRepeatCredentialOperation(t *testing.T) {
	f := newWebFixture(t)
	release := make(chan struct{})
	defer close(release)
	j, err := f.m.job("blocked_fixture", "", func(ctx context.Context, _ *relayprofile.Store) (any, error) {
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if w := f.request("GET", remoteWebAPI+"/profiles", nil, true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("status blocked")
	}
	if _, err = f.m.job("duplicate", "", func(context.Context, *relayprofile.Store) (any, error) {
		t.Error("duplicate credential job ran")
		return nil, nil
	}); !errors.Is(err, relayprofile.ErrBusy) {
		t.Fatal("duplicate admitted")
	}
	if w := f.request("GET", remoteWebAPI+"/jobs/"+j.ID, nil, false); w.Code != 401 {
		t.Fatal("job result unauthenticated")
	}
}
func TestRemoteWebNativeTicketAndAssets(t *testing.T) {
	f := newWebFixture(t)
	events := &nativeEvents{webTicket: f.m.auth.IssueTicket}
	native := httptest.NewServer(nativeControlHandler(strings.Repeat("a", 64), func(string) (*app, bool) { return nil, false }, events))
	defer native.Close()
	link, err := requestWebTicket(context.Background(), nativeBootstrap{1, native.URL, strings.Repeat("a", 64)})
	if err != nil || link != f.origin+"/remote/" {
		t.Fatal("native activation")
	}
	_, err = requestWebTicket(context.Background(), nativeBootstrap{1, native.URL, strings.Repeat("b", 64)})
	if err == nil {
		t.Fatal("native credential bypass")
	}
	w := f.request("GET", "/remote/", nil, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pairing-profile") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("management assets", w.Code)
	}
	w = f.request("GET", "/remote/app.js", nil, false)
	if w.Code != 200 || strings.Contains(w.Body.String(), "innerHTML") || strings.Contains(w.Body.String(), "localStorage") {
		t.Fatal("unsafe management script")
	}
}

func TestRemoteWebStatusAndConnectionLockOrdering(t *testing.T) {
	f := newWebFixture(t)
	f.ready("owner", []string{"module_a"})
	a := f.hub.devices["module_a"].Runtime
	a.remoteConnectionState = func() string {
		f.hub.mu.RLock()
		manager := f.hub.remoteHub
		f.hub.mu.RUnlock()
		if manager == nil {
			return "disabled"
		}
		return manager.Snapshot().State
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			a.remoteCallStatus(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/remote-calls/status", nil))
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := f.m.changeConnection(context.Background(), f.store, "owner", false); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("status/profile switch lock cycle")
	}
}
func TestRemoteWebBrowserPreview(t *testing.T) {
	if os.Getenv("DJONEHUB_MAC_WEB_PREVIEW") != "1" {
		t.Skip("opt-in temporary browser fixture")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newWebFixtureAt(t, "http://"+listener.Addr().String())
	f.ready("home-demo", []string{"module_a", "module_b"})
	f.work("/connection", map[string]any{"profile": "home-demo", "auto_start": true})
	_, _ = f.phone("home-demo", []string{"module_a"})
	server := &http.Server{Handler: f.handler, ReadHeaderTimeout: 3 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	t.Log("PREVIEW_URL=" + f.origin + "/remote/")
	select {
	case <-time.After(5 * time.Minute):
	case <-t.Context().Done():
	}
}

func TestRemoteWebRealRelayConnectionWaitsForPublication(t *testing.T) {
	if !opus.Available() {
		t.Skip("remoteopus integration")
	}
	f := newWebFixture(t)
	f.ready("owner", []string{"module_a"})
	manager, err := startRemoteHubWithStore(f.m.ctx, "owner", "fixture-native-control", f.store, f.hub.remoteDial)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	time.Sleep(25 * time.Millisecond)
	if f.signalingConnections.Load() != 0 {
		t.Fatal("unpublished profile opened signaling")
	}
	f.hub.mu.Lock()
	f.hub.remoteHub = manager
	f.hub.mu.Unlock()
	f.m.setActive("owner")
	manager.activate()
	deadline := time.Now().Add(3 * time.Second)
	for manager.Snapshot().State != "online" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if manager.Snapshot().State != "online" || f.signalingConnections.Load() != 1 {
		t.Fatal("published profile did not authenticate WSS")
	}
	if !manager.RetireIdle() {
		t.Fatal("could not retire idle live connection")
	}
	manager.Close()
	if manager.Snapshot().State != "stopped" {
		t.Fatal("live connection still active")
	}
}

func TestRemoteWebSameAliasModulesHaveStableOrder(t *testing.T) {
	f := newWebFixture(t)
	f.hub.devices["module_c"] = &managedUSBDevice{ID: "module_c", Alias: "同名模块", State: "offline"}
	for i := 0; i < 100; i++ {
		items := f.hub.summaries()
		if len(items) != 3 || items[0].ID != "module_a" || items[1].ID != "module_b" || items[2].ID != "module_c" {
			t.Fatal("ambiguous module order")
		}
	}
}

// Browser fixture: both UIs share only temporary identities and a memory Vault.
// Capture the actual authorization response privately for file-upload QA; never
// log its contents or retain it outside this test directory.
func TestRemoteWebDualBrowserPreview(t *testing.T) {
	if os.Getenv("DJONEHUB_DUAL_WEB_PREVIEW") != "1" {
		t.Skip("opt-in browser fixture")
	}
	duration := 8 * time.Minute // Leave cleanup time before go test's default timeout.
	if value := os.Getenv("DJONEHUB_DUAL_WEB_PREVIEW_DURATION"); value != "" {
		var err error
		duration, err = time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > 8*time.Minute {
			t.Fatal("preview duration must be positive and at most eight minutes")
		}
	}
	macListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		macListener.Close()
		t.Fatal(err)
	}
	f := newWebFixtureAt(t, "http://"+macListener.Addr().String())
	f.m.connect = func(ctx context.Context, id string, store *relayprofile.Store) (*remoteHubService, error) {
		return startRemoteHubWithStore(ctx, id, "fixture-native-control", store, f.hub.remoteDial)
	}
	f.ready("home-demo", []string{"module_a", "module_b"})
	f.work("/connection", map[string]any{"profile": "home-demo", "auto_start": true})
	phone, _ := f.phone("home-demo", []string{"module_a"})
	config, err := f.store.ConnectionConfig(t.Context(), "home-demo")
	if err != nil {
		t.Fatal(err)
	}
	config.AccessToken, config.IdentityID, config.Kind, config.HubID = phone.AccessToken, phone.Principal.ID, phone.Principal.Kind, phone.Principal.HubID
	phoneSignal, err := callsignal.Dial(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer phoneSignal.Close()
	directory := t.TempDir()
	adminOrigin := "http://" + adminListener.Addr().String()
	auth, err := webadmin.Open(webadmin.Options{Directory: filepath.Join(directory, "admin"), Origin: adminOrigin, Tenant: "tenant", CookieName: "fixture_dual_admin", LoopbackOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	admin, err := relayadmin.New(relayadmin.Options{Auth: auth, Identity: f.relay, Directory: filepath.Join(directory, "admin"), RelayOrigin: f.relayOrigin, Build: "dual-browser-fixture", RuntimeStatus: func() relaystatus.Observation {
		return relaystatus.Observe(f.relayService.RuntimeSnapshot(t.Context()), time.Now())
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var setup struct {
		Token string `json:"token"`
	}
	setupData, setupErr := os.ReadFile(auth.SetupPath())
	if setupErr != nil || json.Unmarshal(setupData, &setup) != nil {
		t.Fatal("fixture setup")
	}
	body, _ := json.Marshal(map[string]string{"token": setup.Token, "username": "preview", "password": "preview-test-password"})
	req := httptest.NewRequest("POST", adminOrigin+"/admin/api/session/setup", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set("Origin", adminOrigin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	output := filepath.Join(directory, "browser-authorization.json")
	adminHTTP := &http.Server{ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		admin.ServeHTTP(recorder, r)
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/authorization") && recorder.Code == 200 {
			var authorization identity.EnrollmentAuthorization
			if json.Unmarshal(recorder.Body.Bytes(), &authorization) != nil || authorization.Version != 2 {
				t.Error("invalid authorization response")
			} else if err := os.WriteFile(output, recorder.Body.Bytes(), 0600); err != nil {
				t.Error("fixture authorization capture failed")
			}
		}
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})}
	macHTTP := &http.Server{Handler: f.handler, ReadHeaderTimeout: 3 * time.Second}
	go macHTTP.Serve(macListener)
	go adminHTTP.Serve(adminListener)
	defer macHTTP.Close()
	defer adminHTTP.Close()
	t.Log("MAC_URL=" + f.origin + "/remote/")
	t.Log("ADMIN_URL=" + adminOrigin + "/admin/")
	t.Log("NATIVE_RELAY_URL=" + f.relayOrigin)
	t.Log("AUTHORIZATION_PATH=" + output)
	select {
	case <-time.After(duration):
	case <-t.Context().Done():
	}
}
