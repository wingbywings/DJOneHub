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
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/turncred"
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
	config := PeerConfig{CallUUID: "native-interop", Generation: 1, Trickle: true, Control: true}
	var turnFixture *rotatingTURN
	if os.Getenv("DJONEHUB_IOS_TURN") == "1" {
		turnFixture = newRotatingTURN(t)
		s := turnFixture.initial.Servers[0]
		config.RelayOnly = true
		config.ICEServers = []webrtc.ICEServer{{URLs: s.URLs, Username: s.Username, Credential: s.Credential}}
	}
	p, err := newPeer(ctx, config, settings)
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
	mux.HandleFunc("GET /ice", func(w http.ResponseWriter, r *http.Request) {
		credentials := turncred.Credentials{Servers: []turncred.Server{}}
		if turnFixture != nil {
			credentials = turnFixture.initial
			if r.URL.Query().Get("renew") == "1" {
				var err error
				credentials, err = turnFixture.issue()
				if err != nil {
					http.Error(w, "issuance failed", 500)
					return
				}
			}
		}
		json.NewEncoder(w).Encode(credentials)
	})
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
	mux.HandleFunc("GET /restart-offer", func(w http.ResponseWriter, r *http.Request) {
		var offer RestartDescription
		var err error
		if turnFixture != nil {
			credentials, fetchErr := turnFixture.issue()
			if fetchErr != nil {
				http.Error(w, "issuance failed", 500)
				return
			}
			offer, err = p.CreateRestartOfferWithICE(r.Context(), credentials)
		} else {
			offer, err = p.CreateRestartOffer(r.Context())
		}
		if err != nil {
			http.Error(w, "restart failed", 409)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"negotiation_id": offer.ID, "sdp": offer.Description.SDP.SDP})
	})
	mux.HandleFunc("POST /restart-answer", func(w http.ResponseWriter, r *http.Request) {
		var answer struct {
			ID  uint64 `json:"negotiation_id"`
			SDP string `json:"sdp"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&answer) != nil ||
			p.AcceptRestartAnswer(RestartDescription{ID: answer.ID, Description: Description{CallUUID: "native-interop", Generation: 1, SDP: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}}}) != nil {
			http.Error(w, "restart answer rejected", 400)
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
	mux.HandleFunc("POST /control", func(w http.ResponseWriter, r *http.Request) {
		if p.SendControl([]byte(`{"type":"fixture_state"}`)) != nil {
			http.Error(w, "control unavailable", 409)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /control-received", func(w http.ResponseWriter, r *http.Request) {
		select {
		case data := <-p.ControlMessages():
			var m callsignal.Message
			if callsignal.Decode(data, &m) != nil || m.ValidateControl("installation") != nil {
				http.Error(w, "invalid control", 400)
				return
			}
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST /quiesce", func(w http.ResponseWriter, r *http.Request) {
		p.Quiesce()
		if p.Activate() == nil {
			http.Error(w, "audio reactivated", 500)
			return
		}
		if p.SendControl([]byte(`{"type":"fixture_ended"}`)) != nil {
			http.Error(w, "control closed with audio", 500)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /complete", func(w http.ResponseWriter, r *http.Request) {
		if !p.TransportConnected() {
			http.Error(w, "transport not connected", 409)
			return
		}
		if turnFixture != nil {
			pair, err := p.pc.GetSenders()[0].Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil || pair == nil || pair.Local.Typ != webrtc.ICECandidateTypeRelay || pair.Remote.Typ != webrtc.ICECandidateTypeRelay || !turnFixture.usedAll() {
				http.Error(w, "fresh TURN allocations not verified", 409)
				return
			}
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
