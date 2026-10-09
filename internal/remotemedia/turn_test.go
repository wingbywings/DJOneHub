//go:build cgo && remoteopus

package remotemedia

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/turncred"
	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

func TestOpusMediaThroughForcedTURNRelay(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			secret := strings.Repeat("k", 43)
			config := turn.ServerConfig{Realm: "djonehub-test", AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
				stamp, _, ok := strings.Cut(ra.Username, ":")
				expiry, err := strconv.ParseInt(stamp, 10, 64)
				if !ok || err != nil || expiry <= time.Now().Unix() {
					return "", nil, false
				}
				mac := hmac.New(sha1.New, []byte(secret))
				mac.Write([]byte(ra.Username))
				password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
				return ra.Username, turn.GenerateAuthKey(ra.Username, ra.Realm, password), true
			}}
			generator := &turn.RelayAddressGeneratorStatic{RelayAddress: net.ParseIP("127.0.0.1"), Address: "127.0.0.1"}
			address := ""
			if transport == "udp" {
				listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.LocalAddr().String()
				config.PacketConnConfigs = []turn.PacketConnConfig{{PacketConn: listener, RelayAddressGenerator: generator}}
			} else {
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.Addr().String()
				config.ListenerConfigs = []turn.ListenerConfig{{Listener: listener, RelayAddressGenerator: generator}}
			}
			server, err := turn.NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			issuer, err := turncred.New(turncred.Config{URLs: []string{"turn:" + address + "?transport=" + transport}, Secret: secret, RelayOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			credentials, err := issuer.Issue(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			s := credentials.Servers[0]
			servers := []webrtc.ICEServer{{URLs: s.URLs, Username: s.Username, Credential: s.Credential}}
			testPeerAudioTransport(t, servers, true)
			probeCtx, stopProbe := context.WithTimeout(context.Background(), 5*time.Second)
			result := probeRelay(probeCtx, servers, loopbackSettings())
			stopProbe()
			if !result.Connected || !result.SelectedRelayPair || result.RelayCandidates[0] == 0 || result.RelayCandidates[1] == 0 {
				t.Fatal("authenticated relay diagnostic failed", result)
			}
			servers[0].Credential = "invalid-proof"
			probeCtx, stopProbe = context.WithTimeout(context.Background(), 500*time.Millisecond)
			result = probeRelay(probeCtx, servers, loopbackSettings())
			stopProbe()
			if result.Connected || result.SelectedRelayPair || result.RelayCandidates != [2]uint32{} {
				t.Fatal("wrong TURN proof passed diagnostics", result)
			}
		})
	}
}

// A black-holed ICE server must not hold the offer until gathering times out.
// Real signaling forwards later candidates; the full-gather helper remains
// available for existing peer-to-peer fixtures without a signaling channel.
func TestTrickleOfferDoesNotWaitForUnresponsiveICEServer(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	config := PeerConfig{CallUUID: "trickle-call", Generation: 1, Trickle: true, ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:" + blackhole.LocalAddr().String()}}}}
	peer, err := newPeer(ctx, config, loopbackSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	offerCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	offer, err := peer.CreateOffer(offerCtx)
	if err != nil || offer.SDP.SDP == "" {
		t.Fatal("offer blocked on ICE gathering", err)
	}
	if peer.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete {
		t.Fatal("fixture did not keep ICE gathering pending")
	}
	select {
	case candidate := <-peer.Candidates():
		if candidate.CallUUID != config.CallUUID {
			t.Fatal("candidate lost binding")
		}
	case <-ctx.Done():
		t.Fatal("trickle candidate was not emitted")
	}
}
