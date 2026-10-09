package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/remotecall"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

type nativeTestMedia struct {
	*sessionTestMedia
	bridge *remotemedia.HostBridge
}

func (m *nativeTestMedia) Host() *remotemedia.HostBridge { return m.bridge }
func (m *nativeTestMedia) Close()                        { m.bridge.Close(); m.sessionTestMedia.Close() }
func setupNativeSession(t *testing.T) (*app, *remotecall.Session, *nativeTestMedia, http.Handler) {
	t.Helper()
	a, call := remoteIncoming(t)
	a.usbLocator = &usbDeviceLocator{VendorID: 1, ProductID: 2, LocationID: 3}
	a.moduleVoiceReady = true
	binding := backendFor(a, call).owner.binding
	bridge, err := remotemedia.NewHostBridge(call.UUID, binding.Generation)
	if err != nil {
		t.Fatal(err)
	}
	m := &nativeTestMedia{&sessionTestMedia{done: make(chan struct{})}, bridge}
	s, err := a.startRemoteSession(context.Background(), binding, nil, func(context.Context) (remotecall.Media, error) { return m, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Cancel(); waitSession(t, s) })
	sessionCommand(t, s, "answer")
	deadline := time.Now().Add(time.Second)
	for !a.callMatches(call.UUID, "active") {
		if time.Now().After(deadline) {
			t.Fatal("cellular answer timeout")
		}
		time.Sleep(time.Millisecond)
	}
	return a, s, m, nativeControlHandler("control-secret", func(id string) (*app, bool) { return a, id == "" })
}
func nativeRequest(handler http.Handler, method, path, token, hostID, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12000"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-DJOneHub-Host-ID", hostID)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func TestNativeControlAuthenticationAndNoPublicCredential(t *testing.T) {
	a, _, _, handler := setupNativeSession(t)
	for _, token := range []string{"", "wrong"} {
		if got := nativeRequest(handler, "GET", "/config", token, "host", ""); got.Code != 401 {
			t.Fatal(got.Code)
		}
	}
	for _, tc := range []struct{ address, origin string }{{"192.0.2.1:10", ""}, {"127.0.0.1:10", "http://localhost"}} {
		r := httptest.NewRequest("GET", "/config", nil)
		r.RemoteAddr = tc.address
		r.Header.Set("Authorization", "Bearer control-secret")
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	private := nativeRequest(handler, "GET", "/config", "control-secret", "host", "")
	if private.Code != 200 || !strings.Contains(private.Body.String(), "remote_media_token") || private.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(private.Body.String())
	}
	public := remoteRequest(a, "GET", "/api/calls/audio/host/config", "")
	if strings.Contains(public.Body.String(), "remote_media_token") {
		t.Fatal("public endpoint leaked token")
	}
	if got := nativeRequest(handler, "GET", "/config", "control-secret", "second-host", ""); got.Code != 409 {
		t.Fatal("second host claimed same generation")
	}
	if got := nativeRequest(handler, "GET", "/config?device_id=other", "control-secret", "host", ""); got.Code != 409 {
		t.Fatal("unknown device retargeted")
	}
}
func TestNativeReadyRequiresBridgeAndFreshBoundReport(t *testing.T) {
	a, s, m, handler := setupNativeSession(t)
	configResponse := nativeRequest(handler, "GET", "/config", "control-secret", "host", "")
	var config audioHostConfigResponse
	if err := json.Unmarshal(configResponse.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	report := nativeHostReport{CallUUID: config.CallUUID, Generation: config.MediaGeneration, Sequence: 1, Running: true}
	post := func(body nativeHostReport, host string) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		return nativeRequest(handler, "POST", "/status", "control-secret", host, string(data))
	}
	if got := post(report, "host"); got.Code != 409 {
		t.Fatal("ready accepted before bridge connection")
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+config.RemoteMediaURL, http.Header{"Authorization": {"Bearer " + config.RemoteMediaToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	if got := post(report, "other-host"); got.Code != 409 {
		t.Fatal("non-owner host forged readiness")
	}
	wrong := report
	wrong.Generation++
	if got := post(wrong, "host"); got.Code != 409 {
		t.Fatal("stale binding accepted")
	}
	if got := post(report, "host"); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	sessionState(t, s, "awaiting_audio")
	host := a.nativeRemoteHost()
	if host == nil || !host.ready(s.Snapshot().Binding) {
		t.Fatal("authenticated host not ready")
	}
	if got := post(report, "host"); got.Code != 409 {
		t.Fatal("replayed heartbeat extended lease")
	}
	host.mu.Lock()
	host.seen = time.Now().Add(-3 * time.Second)
	host.mu.Unlock()
	if host.ready(s.Snapshot().Binding) {
		t.Fatal("expired health proof retained readiness")
	}
	report.Sequence = 2
	report.Running = false
	if got := post(report, "host"); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if m.bridge.Ready() {
		t.Fatal("stop report did not close bridge")
	}
	s.Cancel()
	waitSession(t, s)
	if got := post(nativeHostReport{config.CallUUID, config.MediaGeneration, 3, true}, "host"); got.Code != 409 {
		t.Fatal("ended generation resurrected")
	}
}
func TestNativeBootstrapPermissionsRotationAndLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "host.json")
	closeFirst, err := startNativeControl(path, func(string) (*app, bool) { return nil, false })
	if err != nil {
		t.Fatal(err)
	}
	defer closeFirst()
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	data, _ := os.ReadFile(path)
	var first nativeBootstrap
	if err := json.Unmarshal(data, &first); err != nil || first.Version != 1 || len(first.Token) != 64 || !strings.HasPrefix(first.BaseURL, "http://127.0.0.1:") {
		t.Fatal("bad bootstrap metadata")
	}
	response, err := http.Get(first.BaseURL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal(response.StatusCode)
	}
	if closeOther, err := startNativeControl(path, func(string) (*app, bool) { return nil, false }); err == nil {
		closeOther()
		t.Fatal("second listener replaced active bootstrap")
	}
	closeFirst()
	closeSecond, err := startNativeControl(path, func(string) (*app, bool) { return nil, false })
	if err != nil {
		t.Fatal(err)
	}
	defer closeSecond()
	newData, _ := os.ReadFile(path)
	if bytes.Equal(data, newData) {
		t.Fatal("bootstrap did not rotate")
	}
	closeFirst()
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, newData) {
		t.Fatal("old shutdown deleted newer bootstrap")
	}
	closeSecond()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("bootstrap not removed")
	}
}
func TestNativeBootstrapRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if close, err := startNativeControl(filepath.Join(dir, "host.json"), nil); err == nil {
		close()
		t.Fatal("public directory accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "host.json")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if close, err := startNativeControl(path, nil); err == nil {
		close()
		t.Fatal("symlink accepted")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatal("symlink target changed")
	}
}
