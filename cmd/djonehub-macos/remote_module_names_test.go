package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func TestRemoteModuleNamesUseCurrentRemarksWithinAuthorizedScope(t *testing.T) {
	h := &usbDeviceHub{aliases: map[string]string{"offline": "离线备注", "unscoped": "越权备注"}, devices: map[string]*managedUSBDevice{"online": {Alias: "移动主卡"}}}
	ids := []string{"online", "offline", "unknown"}
	names := h.remoteModuleNames(ids)
	if len(names) != 3 || names[0].Alias != "移动主卡" || names[1].Alias != "离线备注" || names[2].Alias != "" {
		t.Fatalf("names: %+v", names)
	}
	h.mu.Lock()
	h.devices["online"].Alias = "新备注"
	h.mu.Unlock()
	if h.remoteModuleNames(ids)[0].Alias != "新备注" {
		t.Fatal("remarks were snapshotted instead of refreshed")
	}
	if remoteModuleAlias(" \n主\u202e卡\t ") != "主卡" || len([]rune(remoteModuleAlias(strings.Repeat("卡", 90)))) != 80 {
		t.Fatal("unsafe legacy remark not normalized")
	}
}

func TestRemoteModuleNamesPublisherRefreshesAndStopsWithSocket(t *testing.T) {
	type publication struct {
		token   string
		modules []identity.ModuleChoice
	}
	requests := make(chan publication, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/v1/module-names" {
			t.Error("invalid publication route")
		}
		var body struct {
			Modules []identity.ModuleChoice `json:"modules"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- publication{r.Header.Get("Authorization"), body.Modules}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":true}`))
	}))
	defer server.Close()
	var mu sync.RWMutex
	token, name := strings.Repeat("a", 43), "旧备注"
	access := func() string { mu.RLock(); defer mu.RUnlock(); return token }
	names := func() []identity.ModuleChoice {
		mu.RLock()
		defer mu.RUnlock()
		return []identity.ModuleChoice{{ID: "module", Alias: name}}
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		publishRemoteModuleNamesEvery(context.Background(), done, server.URL, relayclient.Options{AllowInsecureLoopback: true}, access, names, 20*time.Millisecond)
	}()
	receive := func() publication {
		t.Helper()
		select {
		case p := <-requests:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("no publication")
			return publication{}
		}
	}
	first := receive()
	if first.modules[0].Alias != "旧备注" {
		t.Fatal("first names")
	}
	mu.Lock()
	token = strings.Repeat("b", 43)
	name = "新备注"
	mu.Unlock()
	next := receive()
	if next.modules[0].Alias != "新备注" || next.token != "Bearer "+strings.Repeat("b", 43) {
		t.Fatal("publisher retained stale names or access token")
	}
	close(done)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher continued after socket close")
	}
}
