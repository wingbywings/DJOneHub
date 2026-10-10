//go:build cgo && remoteopus

package remotemedia

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// Opt-in fixture consumed by the native iOS simulator test. Only loopback
// HTTP is used; only synthetic tone PCM, no microphone or telephone audio.
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
	host := connectPeerHost(t, p)
	// Drain any phone uplink so the native-host queue cannot block the fixture.
	_ = host.SetReadDeadline(time.Time{})
	go func() {
		for {
			if _, _, err := host.ReadMessage(); err != nil {
				return
			}
		}
	}()
	// Feed the host before negotiation too; capture remains gated until /audio.
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for sequence := uint64(0); ; sequence++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			frame := Frame{Direction: FromModem, Generation: 1, Sequence: sequence, SampleIndex: sequence * FrameSamples}
			for i := range frame.PCM {
				frame.PCM[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(frame.SampleIndex+uint64(i))/8000))
			}
			data, _ := frame.MarshalBinary()
			_ = host.SetWriteDeadline(time.Now().Add(time.Second))
			if host.WriteMessage(websocket.BinaryMessage, data) != nil {
				return
			}
		}
	}()
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
	mux.HandleFunc("POST /audio", func(w http.ResponseWriter, r *http.Request) {
		if !p.TransportConnected() {
			http.Error(w, "transport not connected", 409)
			return
		}
		state := Readiness{CallUUID: "native-interop", Generation: 1, Revision: 1, CellularActive: true, ModuleReady: true, IOSAudioActive: true}
		if p.UpdateReadiness(state) != nil || p.Activate() != nil {
			http.Error(w, "activation failed", 409)
			return
		}

		w.WriteHeader(204)
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
		send, _ := p.Stats()
		if send.EncodedPackets < 5 || send.SignalFrames < 10 {
			t.Fatalf("synthetic audio was not sent: %+v", send)
		}
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("native iOS companion did not complete")
	}
}
