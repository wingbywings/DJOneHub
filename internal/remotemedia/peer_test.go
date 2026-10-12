//go:build cgo && remoteopus

package remotemedia

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func loopbackSettings() webrtc.SettingEngine {
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	return settings
}
func connectPeerHost(t *testing.T, p *Peer) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(p.Host())
	t.Cleanup(server.Close)
	header := http.Header{"Authorization": []string{"Bearer " + p.Host().Binding().Token}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	if ready["type"] != "ready" {
		t.Fatal(ready)
	}
	return conn
}
func TestWebRTCPeersCarryRealOpusBetweenHostBridges(t *testing.T) {
	t.Run("complete_gathering", func(t *testing.T) { testPeerAudioTransport(t, nil, false) })
	t.Run("trickle", func(t *testing.T) { testPeerAudioTransport(t, nil, true) })
	t.Run("restart_preserves_live_audio", func(t *testing.T) { testPeerAudioTransport(t, nil, false, true) })
}
func testPeerAudioTransport(t *testing.T, servers []webrtc.ICEServer, trickle bool, restart ...bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := PeerConfig{CallUUID: "test-call", Generation: 7, ICEServers: servers, RelayOnly: len(servers) > 0, Trickle: trickle}
	a, err := newPeer(ctx, config, loopbackSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := newPeer(ctx, config, loopbackSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if trickle {
		iceCtx, stopICE := context.WithCancel(ctx)
		var pumps sync.WaitGroup
		defer func() { stopICE(); pumps.Wait() }()
		for _, pair := range [][2]*Peer{{a, b}, {b, a}} {
			pumps.Add(1)
			go func(source, target *Peer) {
				defer pumps.Done()
				for {
					select {
					case <-iceCtx.Done():
						return
					case candidate, ok := <-source.Candidates():
						if !ok {
							return
						}
						if err := target.AddCandidate(candidate); err != nil {
							t.Errorf("trickle candidate: %v", err)
							cancel()
							return
						}
					}
				}
			}(pair[0], pair[1])
		}
	}
	if !errors.Is(a.Activate(), ErrMediaNotReady) {
		t.Fatal("activated before transport/host")
	}
	offer, err := a.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offer.SDP.SDP, "opus/48000/2") || strings.Contains(offer.SDP.SDP, " nack") || strings.Contains(offer.SDP.SDP, "PCMU") {
		t.Fatalf("unexpected codecs/feedback: %s", offer.SDP.SDP)
	}
	stale := offer
	stale.Generation++
	if _, err := b.AcceptOffer(ctx, stale); !errors.Is(err, ErrSignalBinding) {
		t.Fatal("stale SDP accepted")
	}
	if err := b.AddCandidate(Candidate{CallUUID: "old", Generation: 7}); !errors.Is(err, ErrSignalBinding) {
		t.Fatal("old candidate accepted")
	}
	answer, err := b.AcceptOffer(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AcceptAnswer(answer); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Peer{a, b} {
		select {
		case <-p.Connected():
		case <-ctx.Done():
			t.Fatal("ICE/DTLS connection timeout")
		}
	}
	if len(servers) > 0 {
		for _, p := range []*Peer{a, b} {
			pair, err := p.pc.GetSenders()[0].Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil || pair == nil || pair.Local.Typ != webrtc.ICECandidateTypeRelay || pair.Remote.Typ != webrtc.ICECandidateTypeRelay {
				t.Fatal("TURN test selected a direct path", pair, err)
			}
		}
	}
	hosts := []*websocket.Conn{connectPeerHost(t, a), connectPeerHost(t, b)}
	var writers sync.WaitGroup
	inputCtx, stopInput := context.WithCancel(ctx)
	defer func() { stopInput(); writers.Wait() }()
	for index, conn := range hosts {
		writers.Add(1)
		go func(index int, conn *websocket.Conn) {
			defer writers.Done()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			sequence := uint64(0)
			for {
				select {
				case <-inputCtx.Done():
					return
				case <-ticker.C:
				}
				frame := Frame{Direction: FromModem, Generation: 7, Sequence: sequence, SampleIndex: sequence * 80}
				for i := range frame.PCM {
					frame.PCM[i] = int16(8000 * math.Sin(2*math.Pi*float64(440+index*220)*float64(sequence*80+uint64(i))/8000))
				}
				data, _ := frame.MarshalBinary()
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				if conn.WriteMessage(websocket.BinaryMessage, data) != nil {
					return
				}
				sequence++
			}
		}(index, conn)
	}
	// Native capture may run while ringing, but RTP must remain gated.
	time.Sleep(80 * time.Millisecond)
	for _, p := range []*Peer{a, b} {
		send, receive := p.Stats()
		if send.EncodedPackets != 0 || receive.Packets != 0 {
			t.Fatal("audio escaped before Activate")
		}
		if err := p.Activate(); !errors.Is(err, ErrMediaNotReady) {
			t.Fatal("transport alone authorized media")
		}
		state := Readiness{CallUUID: config.CallUUID, Generation: config.Generation, Revision: 1, CellularActive: true, ModuleReady: true}
		if err := p.UpdateReadiness(state); err != nil {
			t.Fatal(err)
		}
		if !p.AnswerReady() {
			t.Fatal("answer-ready incorrectly waits for iOS activation")
		}
		if err := p.Activate(); !errors.Is(err, ErrMediaNotReady) {
			t.Fatal("media started before CallKit activation")
		}
		state.Revision++
		state.IOSAudioActive = true
		if err := p.UpdateReadiness(state); err != nil {
			t.Fatal(err)
		}
		if err := p.Activate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, conn := range hosts {
		var energy int64
		guard := FrameGuard{Generation: 7, Direction: ToModem}
		for i := 0; i < 12; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("PCM read: %v; peer errors: %v / %v", err, a.Err(), b.Err())
			}
			frame, err := ParseFrame(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := guard.Accept(frame); err != nil {
				t.Fatal(err)
			}
			for _, value := range frame.PCM {
				energy += int64(value) * int64(value)
			}
		}
		if energy < 100000 {
			t.Fatalf("decoded audio is silent: %d", energy)
		}
	}
	for _, p := range []*Peer{a, b} {
		send, receive := p.Stats()
		if send.EncodedPackets < 3 || receive.DecodedPackets < 3 {
			t.Fatalf("send=%+v receive=%+v", send, receive)
		}
		state := p.SignalingState()
		if !state.AudioActive || state.Downlink.SignalFrames == 0 || state.Downlink.PeakPCM16 < 1000 || state.Uplink.DecodedPackets < 3 {
			t.Fatalf("status did not expose both audio directions: %+v", state)
		}
	}
	if len(restart) > 0 && restart[0] {
		hostA, hostB := a.Host().Binding(), b.Host().Binding()
		originalOffer := offer
		for attempt := uint64(1); attempt <= 2; attempt++ {
			previousLocal := a.pc.LocalDescription()
			offer, err := a.CreateRestartOffer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if offer.ID != attempt || !changedICE(previousLocal, offer.Description.SDP) {
				t.Fatal("restart reused ICE credentials")
			}
			if _, err := a.CreateRestartOffer(ctx); !errors.Is(err, ErrRestartState) {
				t.Fatal("overlapping restart accepted", err)
			}
			if err := a.AcceptAnswer(answer); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("initial answer entered restart", err)
			}
			stale := offer
			stale.ID++
			if _, err := b.AcceptRestartOffer(ctx, stale); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("skipped negotiation accepted", err)
			}
			stale = offer
			stale.Description = originalOffer
			if _, err := b.AcceptRestartOffer(ctx, stale); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("old ICE offer accepted", err)
			}
			stale = offer
			stale.Description.Generation++
			if _, err := b.AcceptRestartOffer(ctx, stale); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("foreign media binding accepted", err)
			}
			response, err := b.AcceptRestartOffer(ctx, offer)
			if err != nil {
				t.Fatal(err)
			}
			stale = response
			stale.ID++
			if err := a.AcceptRestartAnswer(stale); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("wrong answer version accepted", err)
			}
			if err := a.AcceptRestartAnswer(response); err != nil {
				t.Fatal(err)
			}
			if err := a.AcceptRestartAnswer(response); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("answer replay accepted", err)
			}
			if _, err := b.AcceptRestartOffer(ctx, offer); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("offer replay accepted", err)
			}
			if err := b.AddCandidate(Candidate{CallUUID: config.CallUUID, Generation: config.Generation}); !errors.Is(err, ErrSignalBinding) {
				t.Fatal("old trickle accepted", err)
			}
			_, beforeA := a.Stats()
			_, beforeB := b.Stats()
			deadline := time.Now().Add(3 * time.Second)
			for {
				_, afterA := a.Stats()
				_, afterB := b.Stats()
				if afterA.DecodedPackets >= beforeA.DecodedPackets+5 && afterB.DecodedPackets >= beforeB.DecodedPackets+5 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("audio failed after restart: %v/%v", a.Err(), b.Err())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if a.Host().Binding() != hostA || b.Host().Binding() != hostB {
				t.Fatal("ICE restart replaced native host lease")
			}
			if !a.SignalingState().AudioActive || !b.SignalingState().AudioActive {
				t.Fatal("ICE restart lost CallKit audio readiness")
			}
		}
	}
	// Real SRTCP feedback must reach the custom Opus encoder (not just be
	// drained by Pion). The report is scoped to this peer's outgoing SSRC.
	parameters := a.pc.GetSenders()[0].GetParameters()
	if err := b.pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 12345, Reports: []rtcp.ReceptionReport{{SSRC: uint32(parameters.Encodings[0].SSRC), LastSequenceNumber: 200000, FractionLost: 26, Jitter: 1440}}}}); err != nil {
		t.Fatal(err)
	}
	feedbackDeadline := time.Now().Add(time.Second)
	for a.feedback.snapshot().LossPercent != 11 {
		if time.Now().After(feedbackDeadline) {
			t.Fatal("RTCP feedback did not reach encoder", a.feedback.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
	// Hub mute must discard live RTP even if a faulty client keeps transmitting.
	if err := a.SetMuted(true); err != nil {
		t.Fatal(err)
	}
	muteSend, muteReceive := a.Stats()
	time.Sleep(160 * time.Millisecond)
	mutedSend, mutedReceive := a.Stats()
	if mutedReceive.DecodedPackets != muteReceive.DecodedPackets || mutedReceive.BufferedPackets != 0 || mutedSend.EncodedPackets <= muteSend.EncodedPackets {
		t.Fatal("mute stopped downlink or leaked uplink")
	}
	if err := a.SetMuted(false); err != nil {
		t.Fatal(err)
	}
	unmuteDeadline := time.Now().Add(time.Second)
	for {
		_, receive := a.Stats()
		if receive.DecodedPackets >= mutedReceive.DecodedPackets+3 {
			break
		}
		if time.Now().After(unmuteDeadline) {
			t.Fatal("unmute failed to resume uplink")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A later CallKit deactivation revokes media even while native capture and
	// the other peer keep sending. Old activation snapshots cannot restore it.
	hostBinding := a.Host().Binding()
	if err := a.SuspendAudio(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(a.Activate(), ErrMediaNotReady) {
		t.Fatal("suspension resumed without a new trusted audio sequence")
	}
	if err := a.SetMuted(true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMuted(false); err != nil {
		t.Fatal(err)
	}
	suspendedSend, suspendedReceive := a.Stats()
	time.Sleep(60 * time.Millisecond)
	stillSend, stillReceive := a.Stats()
	if stillSend.EncodedPackets != suspendedSend.EncodedPackets || stillReceive.DecodedPackets != suspendedReceive.DecodedPackets || stillReceive.BufferedPackets != 0 {
		t.Fatal("suspended media kept audio")
	}
	unchanged := a.readiness
	unchanged.Revision++
	if err := a.UpdateReadiness(unchanged); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(a.Activate(), ErrMediaNotReady) {
		t.Fatal("revision alone resumed suspended audio")
	}
	resumed := Readiness{CallUUID: config.CallUUID, Generation: config.Generation, Revision: 4, AudioSequence: 2, CellularActive: true, ModuleReady: true, IOSAudioActive: true}
	if err := a.UpdateReadiness(resumed); err != nil {
		t.Fatal(err)
	}
	if err := a.Activate(); err != nil {
		t.Fatal(err)
	}
	resumeDeadline := time.Now().Add(time.Second)
	for {
		send, receive := a.Stats()
		if send.EncodedPackets >= stillSend.EncodedPackets+3 && receive.DecodedPackets >= stillReceive.DecodedPackets+3 {
			break
		}
		if time.Now().After(resumeDeadline) {
			t.Fatal("real Opus did not resume")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.Host().Binding() != hostBinding {
		t.Fatal("audio interruption replaced host lease")
	}
	paused := Readiness{CallUUID: config.CallUUID, Generation: config.Generation, Revision: 5, CellularActive: true, ModuleReady: true}
	if err := a.UpdateReadiness(paused); err != nil {
		t.Fatal(err)
	}
	before, receive := a.Stats()
	if before.QueuedFrames != 0 || receive.BufferedPackets != 0 {
		t.Fatal("revocation retained buffered audio")
	}
	if !errors.Is(a.Activate(), ErrMediaNotReady) {
		t.Fatal("revoked media activated")
	}
	staleReadiness := paused
	staleReadiness.Revision = 2
	staleReadiness.IOSAudioActive = true
	if !errors.Is(a.UpdateReadiness(staleReadiness), ErrReadinessRevision) {
		t.Fatal("old audio activation restored media")
	}
	time.Sleep(60 * time.Millisecond)
	after, receive := a.Stats()
	if after.EncodedPackets != before.EncodedPackets || after.QueuedFrames != 0 || receive.BufferedPackets != 0 {
		t.Fatal("revoked media still encoded or buffered audio")
	}
	stopInput()
	writers.Wait()
	// Losing the native host terminates the peer and its media workers.
	hosts[0].Close()
	select {
	case <-a.Done():
	case <-ctx.Done():
		t.Fatal("host disconnect did not cancel media")
	}
	a.Close()
	state := a.SignalingState()
	if state.AudioActive || state.Downlink.EncodedPackets != after.EncodedPackets || state.Downlink.SignalFrames == 0 {
		t.Fatalf("ended call lost its audio evidence: %+v", state)
	}
}

func TestReadinessBindingOrderingAndTerminalState(t *testing.T) {
	p, err := newPeer(context.Background(), PeerConfig{CallUUID: "call", Generation: 7}, loopbackSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	state := Readiness{CallUUID: "other", Generation: 7, Revision: 1}
	if !errors.Is(p.UpdateReadiness(state), ErrSignalBinding) {
		t.Fatal("wrong call readiness accepted")
	}
	state.CallUUID = "call"
	state.Revision = 2
	if err := p.UpdateReadiness(state); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateReadiness(state); err != nil {
		t.Fatal("identical readiness retry failed")
	}
	state.Revision = 1
	if !errors.Is(p.UpdateReadiness(state), ErrReadinessRevision) {
		t.Fatal("stale readiness accepted")
	}
	state.Revision = 2
	state.IOSAudioActive = true
	if !errors.Is(p.UpdateReadiness(state), ErrReadinessRevision) {
		t.Fatal("conflicting readiness accepted")
	}
	state.Revision = 3
	state.Ended = true
	if err := p.UpdateReadiness(state); err != nil {
		t.Fatal(err)
	}
	state.Revision = 4
	state.Ended = false
	state.CellularActive = true
	state.ModuleReady = true
	if err := p.UpdateReadiness(state); err == nil {
		t.Fatal("ended peer resurrected")
	}
	if p.AnswerReady() {
		t.Fatal("ended peer answer ready")
	}
}
