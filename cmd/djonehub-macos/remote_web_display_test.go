package main

import (
	"strings"
	"testing"
	"time"
)

func TestDisplayCacheDoesNotLoadVaultAndNeverCrossesProfiles(t *testing.T) {
	f := newWebFixture(t)
	f.ready("owner", []string{"module_a"})
	phone, _ := f.phone("owner", []string{"module_a"})
	f.work("/installations", map[string]any{"profile": "owner", "action": "list"})
	before := f.factoryCalls.Load()
	path := remoteWebAPI + "/profiles/owner/presence"
	if f.request("GET", path, nil, false).Code != 401 {
		t.Fatal("unauthenticated presence cache")
	}
	w := f.request("GET", path, nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), phone.Principal.ID) || strings.Contains(w.Body.String(), phone.AccessToken) {
		t.Fatal("presence cache or credential boundary")
	}
	if f.factoryCalls.Load() != before {
		t.Fatal("GET presence accessed credential store")
	}
	w = f.request("GET", remoteWebAPI+"/profiles/another/presence", nil, true)
	if strings.Contains(w.Body.String(), phone.Principal.ID) || !strings.Contains(w.Body.String(), `"state":"unknown"`) {
		t.Fatal("cache crossed profiles")
	}
	f.m.mu.Lock()
	f.m.presence.CheckedAt = time.Now().Add(-13 * time.Second)
	f.m.mu.Unlock()
	w = f.request("GET", path, nil, true)
	if strings.Contains(w.Body.String(), phone.Principal.ID) || !strings.Contains(w.Body.String(), `"state":"unknown"`) {
		t.Fatal("stale cache returned identity observations")
	}
}

func TestManagementAddressRequiresHTTPSOriginAndSurvivesRefresh(t *testing.T) {
	f := newWebFixture(t)
	f.ready("owner", []string{"module_a"})
	for _, address := range []string{"javascript:alert(1)", "http://admin.test", "https://admin.test/other", "https://user:password@admin.test", "https://admin.test?secret=1"} {
		j := f.work("/profiles/owner/actions", map[string]string{"action": "admin_url", "admin_url": address})
		if j.State != "failed" {
			t.Fatal("unsafe address accepted")
		}
	}
	j := f.work("/profiles/owner/actions", map[string]string{"action": "admin_url", "admin_url": "https://admin.test"})
	if j.State != "completed" {
		t.Fatal(j.Error)
	}
	f.work("/profiles/owner/actions", map[string]string{"action": "refresh"})
	w := f.request("GET", remoteWebAPI+"/profiles", nil, true)
	if !strings.Contains(w.Body.String(), `"admin_url":"https://admin.test"`) {
		t.Fatal("management address lost on refresh")
	}
}
