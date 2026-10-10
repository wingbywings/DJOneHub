package remotemedia

import "strings"

// SignalingState retains the last state before Close. No SDP, candidate
// addresses, identity, TURN credential or raw error is exposed.
type SignalingState struct {
	LocalCandidates         uint64        `json:"local_candidates"`
	LocalRelayCandidates    uint64        `json:"local_relay_candidates"`
	RemoteTrickleCandidates uint64        `json:"remote_trickle_candidates"`
	RemoteRelayCandidates   uint64        `json:"remote_relay_candidates"`
	AnswerAccepted          bool          `json:"answer_accepted"`
	PeerState               string        `json:"peer_state"`
	ICEState                string        `json:"ice_state"`
	GatheringState          string        `json:"gathering_state"`
	AudioActive             bool          `json:"audio_active"`
	Downlink                SenderStats   `json:"downlink"`
	Uplink                  ReceiverStats `json:"uplink"`
	closing                 bool
}

func (p *Peer) SignalingState() SignalingState {
	p.diagnosticMu.Lock()
	state := p.diagnostics
	p.diagnosticMu.Unlock()
	p.mediaMu.Lock()
	state.AudioActive = p.active && p.ctx.Err() == nil
	state.Downlink, state.Uplink = p.Stats()
	p.mediaMu.Unlock()
	return state
}

func relayCandidate(value string) bool {
	fields := strings.Fields(value)
	for i, field := range fields {
		if field == "typ" && i+1 < len(fields) {
			return fields[i+1] == "relay"
		}
	}
	return false
}

func (p *Peer) remoteCandidateAccepted(value string) {
	p.diagnosticMu.Lock()
	defer p.diagnosticMu.Unlock()
	p.diagnostics.RemoteTrickleCandidates++
	if relayCandidate(value) {
		p.diagnostics.RemoteRelayCandidates++
	}
}
