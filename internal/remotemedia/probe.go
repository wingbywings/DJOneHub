package remotemedia

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

// RelayProbe reports fixed stages and counts, never SDP, addresses or secrets.
// Both peers require relay candidates; no cellular/native audio is opened.
type RelayProbe struct {
	Stage             string    `json:"stage"`
	Connected         bool      `json:"connected"`
	RelayCandidates   [2]uint32 `json:"relay_candidates"`
	PeerStates        [2]string `json:"peer_states"`
	ICEStates         [2]string `json:"ice_states"`
	SelectedRelayPair bool      `json:"selected_relay_pair"`
}

func ProbeRelay(ctx context.Context, servers []webrtc.ICEServer) RelayProbe {
	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	return probeRelay(ctx, servers, settings)
}

func probeRelay(parent context.Context, servers []webrtc.ICEServer, settings webrtc.SettingEngine) (result RelayProbe) {
	result.Stage = "peer_init_failed"
	if len(servers) == 0 {
		result.Stage = "turn_not_configured"
		return
	}
	// Avoid native library logs containing deployment addresses or credentials.
	settings.LoggerFactory = &logging.DefaultLoggerFactory{DefaultLogLevel: logging.LogLevelDisabled, Writer: io.Discard}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	config := PeerConfig{CallUUID: uuid.NewString(), Generation: 1, ICEServers: servers, RelayOnly: true, Trickle: true}
	a, err := newPeer(ctx, config, settings)
	if err != nil {
		return
	}
	defer a.Close()
	b, err := newPeer(ctx, config, settings)
	if err != nil {
		return
	}
	defer b.Close()
	peers := [2]*Peer{a, b}
	var counts [2]atomic.Uint32
	defer func() {
		for i, p := range peers {
			result.RelayCandidates[i] = counts[i].Load()
			result.PeerStates[i] = p.pc.ConnectionState().String()
			result.ICEStates[i] = p.pc.ICEConnectionState().String()
		}
	}()
	var workers sync.WaitGroup
	pumpCtx, stopPumps := context.WithCancel(ctx)
	defer func() { stopPumps(); workers.Wait() }()
	failed := make(chan struct{}, 2)
	for i, source := range peers {
		target := peers[1-i]
		workers.Add(1)
		go func(index int, source, target *Peer) {
			defer workers.Done()
			for {
				select {
				case <-pumpCtx.Done():
					return
				case candidate := <-source.Candidates():
					// Relay-only gathering emits only relay candidates.
					counts[index].Add(1)
					err := target.AddCandidate(candidate)
					if err != nil {
						failed <- struct{}{}
						return
					}
				}
			}
		}(i, source, target)
	}
	result.Stage = "offer_failed"
	offer, err := a.CreateOffer(ctx)
	if err != nil {
		return
	}
	result.Stage = "answer_failed"
	answer, err := b.AcceptOffer(ctx, offer)
	if err != nil {
		return
	}
	if a.AcceptAnswer(answer) != nil {
		return
	}
	result.Stage = "transport_timeout"
	for _, p := range peers {
		select {
		case <-p.Connected():
		case <-failed:
			result.Stage = "candidate_failed"
			return
		case <-p.Done():
			if parent.Err() == nil {
				result.Stage = "transport_failed"
			}
			return
		case <-ctx.Done():
			return
		}
	}
	result.Stage = "relay_pair_missing"
	for _, p := range peers {
		pair, err := p.pc.GetSenders()[0].Transport().ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil || pair.Local.Typ != webrtc.ICECandidateTypeRelay || pair.Remote.Typ != webrtc.ICECandidateTypeRelay {
			return
		}
	}
	result.Stage, result.Connected, result.SelectedRelayPair = "connected", true, true
	return
}
