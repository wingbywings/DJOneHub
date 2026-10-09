package remotemedia

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var ErrSignalBinding = errors.New("signaling does not belong to this call/media generation")
var ErrMediaNotReady = errors.New("transport, host, cellular call, module and iOS audio must be ready before media activation")

type PeerConfig struct {
	CallUUID   string
	Generation uint64
	ICEServers []webrtc.ICEServer
	RelayOnly  bool
	Trickle    bool // return SDP immediately; Candidates carries subsequent ICE
}
type Description struct {
	CallUUID   string                    `json:"call_uuid"`
	Generation uint64                    `json:"generation"`
	SDP        webrtc.SessionDescription `json:"sdp"`
}
type Candidate struct {
	CallUUID   string                  `json:"call_uuid"`
	Generation uint64                  `json:"generation"`
	ICE        webrtc.ICECandidateInit `json:"ice"`
}

// Peer connects one authorized call to a single Opus WebRTC track and the
// native loopback host. Construction/signaling never enables audio: the call
// coordinator must explicitly Activate after its answer-ready prerequisites.
// No app HTTP route creates peers yet; the AI path never constructs one.
type Peer struct {
	config        PeerConfig
	pc            *webrtc.PeerConnection
	track         *webrtc.TrackLocalStaticRTP
	bridge        *HostBridge
	codec         *opus.Codec
	sender        *Sender
	receiver      *Receiver
	ctx           context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
	workerMu      sync.Mutex
	closed        bool
	workers       sync.WaitGroup
	mediaMu       sync.Mutex
	active        bool
	readiness     Readiness
	remoteTrack   bool
	signalMu      sync.Mutex
	pendingICE    []webrtc.ICECandidateInit
	candidates    chan Candidate
	connected     chan struct{}
	connectedOnce sync.Once
	errMu         sync.Mutex
	lastError     error
	diagnosticMu  sync.Mutex
	diagnostics   SignalingState
	sequence      uint16
	timestampBase uint32
}

func NewPeer(ctx context.Context, config PeerConfig) (*Peer, error) {
	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	settings.SetICETimeouts(5*time.Second, 10*time.Second, 2*time.Second)
	return newPeer(ctx, config, settings)
}
func newPeer(parent context.Context, config PeerConfig, settings webrtc.SettingEngine) (*Peer, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	codec, err := opus.New(opus.DefaultConfig())
	if err != nil {
		return nil, err
	}
	bridge, err := NewHostBridge(config.CallUUID, config.Generation)
	if err != nil {
		codec.Close()
		return nil, err
	}
	engine := &webrtc.MediaEngine{}
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=0;sprop-stereo=0;maxplaybackrate=8000;maxaveragebitrate=24000"}
	if err = engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: capability, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		codec.Close()
		bridge.Close()
		return nil, err
	}
	registry := &interceptor.Registry{}
	// RTCP reports, without audio NACK/retransmission or video interceptors.
	if err = webrtc.ConfigureRTCPReports(registry); err != nil {
		codec.Close()
		bridge.Close()
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings))
	rtcConfig := webrtc.Configuration{ICEServers: config.ICEServers}
	if config.RelayOnly {
		rtcConfig.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := api.NewPeerConnection(rtcConfig)
	if err != nil {
		codec.Close()
		bridge.Close()
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticRTP(capability, "telephone", config.CallUUID)
	if err != nil {
		_ = pc.Close()
		codec.Close()
		bridge.Close()
		return nil, err
	}
	transceiver, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		_ = pc.Close()
		codec.Close()
		bridge.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	sender, _ := NewSender(config.Generation, codec)
	receiver, _ := NewReceiver(config.Generation, codec, opus.PacketSamples)
	p := &Peer{config: config, pc: pc, track: track, bridge: bridge, codec: codec, sender: sender, receiver: receiver, ctx: ctx, cancel: cancel, candidates: make(chan Candidate, 128), connected: make(chan struct{})}
	p.diagnostics = SignalingState{PeerState: "new", ICEState: "new", GatheringState: "new"}
	var seed [6]byte
	if _, err = rand.Read(seed[:]); err != nil {
		p.Close()
		return nil, err
	}
	p.sequence = binary.BigEndian.Uint16(seed[:2])
	p.timestampBase = binary.BigEndian.Uint32(seed[2:])
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		p.diagnosticMu.Lock()
		p.diagnostics.LocalCandidates++
		if candidate.Typ == webrtc.ICECandidateTypeRelay {
			p.diagnostics.LocalRelayCandidates++
		}
		p.diagnosticMu.Unlock()
		select {
		case p.candidates <- Candidate{config.CallUUID, config.Generation, candidate.ToJSON()}:
		case <-p.ctx.Done():
		default:
			p.fail(errors.New("local ICE candidate queue exhausted"))
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.diagnosticMu.Lock()
		if !p.diagnostics.closing {
			p.diagnostics.PeerState = state.String()
		}
		p.diagnosticMu.Unlock()
		if state == webrtc.PeerConnectionStateConnected {
			p.connectedOnce.Do(func() { close(p.connected) })
		}
		if state == webrtc.PeerConnectionStateFailed {
			p.fail(errors.New("WebRTC transport failed"))
		}
		if state == webrtc.PeerConnectionStateClosed {
			p.cancel()
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		p.diagnosticMu.Lock()
		if !p.diagnostics.closing {
			p.diagnostics.ICEState = state.String()
		}
		p.diagnosticMu.Unlock()
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		p.diagnosticMu.Lock()
		if !p.diagnostics.closing {
			p.diagnostics.GatheringState = state.String()
		}
		p.diagnosticMu.Unlock()
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		p.mediaMu.Lock()
		valid := !p.remoteTrack && track.Kind() == webrtc.RTPCodecTypeAudio && track.Codec().MimeType == webrtc.MimeTypeOpus && track.Codec().ClockRate == 48000
		if valid {
			p.remoteTrack = true
		}
		p.mediaMu.Unlock()
		if !valid {
			p.fail(errors.New("unexpected remote media track"))
			return
		}
		p.startWorker(func() { p.readTrack(track) })
	})
	p.startWorker(func() {
		for {
			if _, _, err := transceiver.Sender().ReadRTCP(); err != nil {
				return
			}
		}
	})
	p.startWorker(p.capture)
	p.startWorker(p.play)
	go func() { <-ctx.Done(); p.Close() }()
	return p, nil
}
func (p *Peer) startWorker(run func()) {
	p.workerMu.Lock()
	defer p.workerMu.Unlock()
	if p.closed {
		return
	}
	p.workers.Add(1)
	go func() { defer p.workers.Done(); run() }()
}
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		p.diagnosticMu.Lock()
		p.diagnostics.closing = true
		p.diagnosticMu.Unlock()
		p.workerMu.Lock()
		p.closed = true
		p.workerMu.Unlock()
		p.cancel()
		p.bridge.Close()
		_ = p.pc.Close()
		p.workers.Wait()
		p.codec.Close()
	})
}
func (p *Peer) fail(err error) {
	p.errMu.Lock()
	if p.lastError == nil {
		p.lastError = err
	}
	p.errMu.Unlock()
	p.cancel()
}
func (p *Peer) Err() error                   { p.errMu.Lock(); defer p.errMu.Unlock(); return p.lastError }
func (p *Peer) Done() <-chan struct{}        { return p.ctx.Done() }
func (p *Peer) Connected() <-chan struct{}   { return p.connected }
func (p *Peer) Candidates() <-chan Candidate { return p.candidates }
func (p *Peer) Host() *HostBridge            { return p.bridge }

// TransportConnected is the pre-ATA prerequisite. The native UAC host may only
// become ready after the cellular call is active; AnswerReady checks both.
func (p *Peer) TransportConnected() bool {
	return p.ctx.Err() == nil && p.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}
func (p *Peer) Activate() error {
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if !p.transportReady() || !p.readiness.CellularActive || !p.readiness.ModuleReady || !p.readiness.IOSAudioActive || p.readiness.Ended {
		return ErrMediaNotReady
	}
	p.active = true
	return nil
}
func (p *Peer) matches(uuid string, generation uint64) bool {
	return uuid == p.config.CallUUID && generation == p.config.Generation
}
func (p *Peer) AddCandidate(candidate Candidate) error {
	if !p.matches(candidate.CallUUID, candidate.Generation) {
		return ErrSignalBinding
	}
	if len(candidate.ICE.Candidate) > 4096 {
		return ErrFrame
	}
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.pc.RemoteDescription() == nil {
		if len(p.pendingICE) >= 128 {
			return errors.New("remote ICE candidate queue exhausted")
		}
		p.pendingICE = append(p.pendingICE, candidate.ICE)
		p.remoteCandidateAccepted(candidate.ICE.Candidate)
		return nil
	}
	err := p.pc.AddICECandidate(candidate.ICE)
	if err == nil {
		p.remoteCandidateAccepted(candidate.ICE.Candidate)
	}
	return err
}
func (p *Peer) remoteDescription(description Description, kind webrtc.SDPType) error {
	if !p.matches(description.CallUUID, description.Generation) {
		return ErrSignalBinding
	}
	if description.SDP.Type != kind || len(description.SDP.SDP) > 65536 {
		return ErrFrame
	}
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if err := p.pc.SetRemoteDescription(description.SDP); err != nil {
		return err
	}
	if kind == webrtc.SDPTypeAnswer {
		p.diagnosticMu.Lock()
		p.diagnostics.AnswerAccepted = true
		p.diagnosticMu.Unlock()
	}
	for _, candidate := range p.pendingICE {
		if err := p.pc.AddICECandidate(candidate); err != nil {
			return err
		}
	}
	p.pendingICE = nil
	return nil
}
func (p *Peer) CreateOffer(ctx context.Context) (Description, error) {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return Description{}, err
	}
	return p.localDescription(ctx, offer)
}
func (p *Peer) AcceptOffer(ctx context.Context, offer Description) (Description, error) {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if err := p.remoteDescription(offer, webrtc.SDPTypeOffer); err != nil {
		return Description{}, err
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return Description{}, err
	}
	return p.localDescription(ctx, answer)
}
func (p *Peer) AcceptAnswer(answer Description) error {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	return p.remoteDescription(answer, webrtc.SDPTypeAnswer)
}
func (p *Peer) localDescription(ctx context.Context, description webrtc.SessionDescription) (Description, error) {
	var done <-chan struct{}
	if !p.config.Trickle {
		done = webrtc.GatheringCompletePromise(p.pc)
	}
	if err := p.pc.SetLocalDescription(description); err != nil {
		return Description{}, err
	}
	if p.config.Trickle {
		return Description{p.config.CallUUID, p.config.Generation, *p.pc.LocalDescription()}, nil
	}
	select {
	case <-ctx.Done():
		return Description{}, ctx.Err()
	case <-p.ctx.Done():
		return Description{}, p.ctx.Err()
	case <-done:
	}
	return Description{p.config.CallUUID, p.config.Generation, *p.pc.LocalDescription()}, nil
}
func (p *Peer) capture() {
	for {
		frame, err := p.bridge.Receive(p.ctx)
		if err != nil {
			if p.ctx.Err() == nil {
				p.fail(err)
			}
			return
		}
		p.mediaMu.Lock()
		if p.active {
			err = p.sender.Push(frame, time.Now())
		}
		p.mediaMu.Unlock()
		if err != nil {
			p.fail(err)
			return
		}
	}
}
func (p *Peer) readTrack(track *webrtc.TrackRemote) {
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			if p.ctx.Err() == nil {
				p.fail(err)
			}
			return
		}
		p.mediaMu.Lock()
		if p.active {
			_ = p.receiver.Push(IncomingRTP{p.config.Generation, uint32(track.SSRC()), packet.SequenceNumber, packet.Timestamp, packet.Payload}, time.Now())
		}
		p.mediaMu.Unlock()
	}
}
func (p *Peer) play() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var now time.Time
		select {
		case <-p.ctx.Done():
			return
		case tick := <-ticker.C:
			// Use the ticker's stable phase during ordinary scheduler jitter.
			// A stalled process must still expire/rebuffer against real time.
			now = tick
			if wall := time.Now(); wall.Sub(tick) > 20*time.Millisecond {
				now = wall
			}
		}
		p.mediaMu.Lock()
		if !p.active {
			p.mediaMu.Unlock()
			continue
		}
		encoded, err := p.sender.Next(now)
		if err == nil && encoded != nil {
			packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: p.sequence, Timestamp: p.timestampBase + encoded.Timestamp}, Payload: encoded.Payload}
			p.sequence++
			err = p.track.WriteRTP(packet)
		}
		var pcm *Frame
		if err == nil {
			pcm, err = p.receiver.Read(now)
		}
		if err == nil && pcm != nil {
			err = p.bridge.Send(*pcm)
		}
		p.mediaMu.Unlock()
		if err != nil {
			p.fail(fmt.Errorf("remote media pump: %w", err))
			return
		}
	}
}
func (p *Peer) Stats() (SenderStats, ReceiverStats) { return p.sender.Stats(), p.receiver.Stats() }

func (p *Peer) transportReady() bool {
	return p.pc.ConnectionState() == webrtc.PeerConnectionStateConnected && p.bridge.Ready()
}
