//go:build cgo && remoteopus

package remotemedia

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"github.com/iniwex5/vohive/internal/turncred"
	"github.com/pion/turn/v5"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The native fixture uses real UDP TURN allocations with short initial REST
// credentials. Test credentials only; never connects to a production relay.
type rotatingTURN struct {
	mu      sync.Mutex
	issuer  *turncred.Issuer
	issued  int
	allowed map[string]bool
	used    map[string]bool
	initial turncred.Credentials
	log     func(string, ...any)
}

func newRotatingTURN(t *testing.T) *rotatingTURN {
	t.Helper()
	fixture := &rotatingTURN{allowed: map[string]bool{}, used: map[string]bool{}, log: t.Logf}
	secret := strings.Repeat("t", 40)
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: "djonehub-rotation-test",
		AuthHandler: func(a *turn.RequestAttributes) (string, []byte, bool) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			stamp, _, _ := strings.Cut(a.Username, ":")
			expires, err := strconv.ParseInt(stamp, 10, 64)
			if err != nil || expires <= time.Now().Unix() || !fixture.allowed[a.Username] {
				return "", nil, false
			}
			if !fixture.used[a.Username] {
				t.Logf("TURN authentication accepted for credential set %d", len(fixture.used)+1)
			}
			fixture.used[a.Username] = true
			mac := hmac.New(sha1.New, []byte(secret))
			mac.Write([]byte(a.Username))
			return a.Username, turn.GenerateAuthKey(a.Username, a.Realm, base64.StdEncoding.EncodeToString(mac.Sum(nil))), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: listener, RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{RelayAddress: net.ParseIP("127.0.0.1"), Address: "127.0.0.1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	fixture.issuer, _ = turncred.New(turncred.Config{URLs: []string{"turn:" + listener.LocalAddr().String() + "?transport=udp"}, Secret: secret, RelayOnly: true})
	// Leave room for slow simulator launches. Near-expiry timing is tested
	// independently by the recovery scheduler's injected clock.
	fixture.initial, _ = fixture.issuer.Issue(time.Now().Add(-turncred.Lifetime + 5*time.Minute))
	fixture.allowed[fixture.initial.Servers[0].Username] = true
	return fixture
}
func (f *rotatingTURN) issue() (turncred.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	f.log("TURN rotation issued credential set %d", f.issued)
	c, err := f.issuer.Issue(time.Now().Add(time.Duration(f.issued) * time.Second))
	if err == nil {
		f.allowed[c.Servers[0].Username] = true
	}
	return c, err
}
func (f *rotatingTURN) usedAll() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for username := range f.allowed {
		if !f.used[username] {
			return false
		}
	}
	return len(f.used) >= 5 // initial + independent Hub/phone credentials twice
}
