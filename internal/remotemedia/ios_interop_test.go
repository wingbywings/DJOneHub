//go:build cgo && remoteopus

package remotemedia

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// Opt-in fixture consumed by the native iOS simulator test. Only loopback
// HTTP is used; no identity/TURN credentials or actual audio are involved.
func TestIOSNativeInteropFixture(t *testing.T) {
	if os.Getenv("DJONEHUB_IOS_INTEROP") != "1" {
		t.Skip("requires companion iOS simulator test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	p, err := newPeer(ctx, PeerConfig{CallUUID: "native-interop", Generation: 1, Trickle: true}, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	offer, err := p.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var candidates []webrtc.ICECandidateInit
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-p.Candidates():
				mu.Lock()
				candidates = append(candidates, c.ICE)
				mu.Unlock()
			}
		}
	}()
	done := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /offer", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(offer.SDP) })
	mux.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
		var sdp webrtc.SessionDescription
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&sdp) != nil || p.AcceptAnswer(Description{CallUUID: "native-interop", Generation: 1, SDP: sdp}) != nil {
			http.Error(w, "answer rejected", 400)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /candidate", func(w http.ResponseWriter, r *http.Request) {
		var ice webrtc.ICECandidateInit
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&ice) != nil || p.AddCandidate(Candidate{CallUUID: "native-interop", Generation: 1, ICE: ice}) != nil {
			http.Error(w, "candidate rejected", 400)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /candidates", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if candidates == nil {
			w.Write([]byte("[]"))
			return
		}
		json.NewEncoder(w).Encode(candidates)
		candidates = nil
	})
	mux.HandleFunc("POST /complete", func(w http.ResponseWriter, r *http.Request) {
		if !p.TransportConnected() {
			http.Error(w, "transport not connected", 409)
			return
		}
		select {
		case done <- struct{}{}:
		default:
		}
		w.WriteHeader(204)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:39096")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	defer server.Close()
	go server.Serve(listener)
	t.Log("iOS interoperability fixture ready on loopback port 39096")
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("native iOS companion did not complete")
	}
}
