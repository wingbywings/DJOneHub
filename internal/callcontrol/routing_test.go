package callcontrol

import (
	"regexp"
	"testing"
)

func TestRoutePriorityAndIsolation(t *testing.T) {
	for _, test := range []struct {
		ai, remote bool
		want       Route
	}{
		{false, false, Local}, {false, true, Remote},
		{true, false, Agent}, {true, true, Agent},
	} {
		config, err := NormalizeRemoteConfig(RemoteConfig{Enabled: test.remote, TargetInstallationID: "phone-1", Revision: 9})
		if err != nil {
			t.Fatal(err)
		}
		route := SelectRoute(test.ai, 7, config)
		if route.Mode != test.want {
			t.Fatalf("ai=%v remote=%v: %#v", test.ai, test.remote, route)
		}
		if route.Mode != Remote && (route.TargetInstallationID != "" || route.RingTimeoutSeconds != 0 || route.CallerDisplay != "") {
			t.Fatalf("non-remote route leaked remote invitation data: %#v", route)
		}
		if route.Mode == Agent && route.ConfigRevision != 7 {
			t.Fatalf("AI revision: %#v", route)
		}
		if route.Mode == Remote && (route.ConfigRevision != 9 || route.RingTimeoutSeconds != 45) {
			t.Fatalf("remote snapshot: %#v", route)
		}
	}
}

func TestInvalidRemoteConfiguration(t *testing.T) {
	for _, config := range []RemoteConfig{
		{Enabled: true}, {TargetInstallationID: "../phone"},
		{RingTimeoutSeconds: -1}, {RingTimeoutSeconds: 9}, {RingTimeoutSeconds: 91},
		{CallerDisplay: "secret"},
	} {
		if _, err := NormalizeRemoteConfig(config); err == nil {
			t.Fatalf("accepted %#v", config)
		}
	}
}

func TestCallUUIDsAreVersionFourAndDistinct(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewCallUUID()
		if !pattern.MatchString(id) || seen[id] {
			t.Fatalf("invalid or repeated UUID: %s", id)
		}
		seen[id] = true
	}
}
