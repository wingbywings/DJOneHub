package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func TestRemoteModuleRoutesReflectSelectRoute(t *testing.T) {
	h := newUSBDeviceHub()
	ids := []string{"offline", "ai", "phone"}
	ai := &app{deviceID: "ai", demo: true}
	ai.ensureVoiceAgent().mu.Lock()
	ai.ensureVoiceAgent().state.Enabled = true
	ai.ensureVoiceAgent().mu.Unlock()
	phone := &app{deviceID: "phone", demo: true}
	remote, err := callcontrol.NormalizeRemoteConfig(callcontrol.RemoteConfig{
		Enabled: true, TargetInstallationID: "phone_install_1", RingTimeoutSeconds: 45, CallerDisplay: "number",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := phone.ensureRemoteCalls().save(remote); err != nil {
		t.Fatal(err)
	}
	h.devices["ai"] = &managedUSBDevice{ID: "ai", State: "ready", Runtime: ai}
	h.devices["phone"] = &managedUSBDevice{ID: "phone", State: "ready", Runtime: phone}
	h.devices["offline"] = &managedUSBDevice{ID: "offline", State: "offline"}

	routes := h.remoteModuleRoutes(ids)
	if len(routes) != 3 {
		t.Fatalf("routes=%d", len(routes))
	}
	byID := map[string]callcontrol.ModuleRouteEntry{}
	for _, route := range routes {
		byID[route.ID] = route
	}
	if byID["offline"].Known || byID["offline"].EffectiveRoute != "" {
		t.Fatalf("offline must stay unknown: %+v", byID["offline"])
	}
	if !byID["ai"].Known || byID["ai"].EffectiveRoute != string(callcontrol.Agent) || !byID["ai"].AIEnabled {
		t.Fatalf("ai route: %+v", byID["ai"])
	}
	if !byID["phone"].Known || byID["phone"].EffectiveRoute != string(callcontrol.Remote) ||
		byID["phone"].TargetInstallationID != "phone_install_1" || !byID["phone"].RemoteEnabled {
		t.Fatalf("phone route: %+v", byID["phone"])
	}
}

func TestRemoteModuleRoutesPublisherRefreshesAndStopsWithSocket(t *testing.T) {
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/v1/module-routes" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Modules []callcontrol.ModuleRouteEntry `json:"modules"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || len(payload.Modules) == 0 {
			http.Error(w, "bad", 400)
			return
		}
		puts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":true}`))
	}))
	defer server.Close()

	done, stopped := make(chan struct{}), make(chan struct{})
	access := func() string { return "token" }
	routes := func() []callcontrol.ModuleRouteEntry {
		return []callcontrol.ModuleRouteEntry{{ID: "module", EffectiveRoute: "local", Known: true}}
	}
	go func() {
		defer close(stopped)
		publishRemoteModuleRoutesEvery(context.Background(), done, server.URL, relayclient.Options{AllowInsecureLoopback: true}, access, routes, 20*time.Millisecond)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for puts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if puts.Load() < 2 {
		t.Fatal("publisher did not refresh")
	}
	close(done)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not stop with socket")
	}
}
