package remotemedia

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/iniwex5/vohive/internal/turncred"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// RestartDescription versions negotiation independently of the call/media
// binding. ICE restart must not replace the cellular call or native host lease.
// It contains a fully gathered SDP: restart candidates are delivered atomically
// with their credentials, not replayed from a previous trickle generation.
type RestartDescription struct {
	ID          uint64      `json:"negotiation_id"`
	Description Description `json:"description"`
}

var ErrRestartState = errors.New("ICE restart requires an established, stable peer")

func changedICE(previous *webrtc.SessionDescription, next webrtc.SessionDescription) bool {
	if previous == nil {
		return false
	}
	var oldSDP, newSDP sdp.SessionDescription
	if oldSDP.Unmarshal([]byte(previous.SDP)) != nil || newSDP.Unmarshal([]byte(next.SDP)) != nil ||
		oldSDP.Origin.SessionID != newSDP.Origin.SessionID || newSDP.Origin.SessionVersion <= oldSDP.Origin.SessionVersion {
		return false
	}
	if len(oldSDP.MediaDescriptions) != len(newSDP.MediaDescriptions) {
		return false
	}
	credentials := func(description webrtc.SessionDescription) (string, string, bool) {
		var parsed sdp.SessionDescription
		if parsed.Unmarshal([]byte(description.SDP)) != nil || (len(parsed.MediaDescriptions) != 1 && len(parsed.MediaDescriptions) != 2) || parsed.MediaDescriptions[0].MediaName.Media != "audio" || (len(parsed.MediaDescriptions) == 2 && parsed.MediaDescriptions[1].MediaName.Media != "application") {
			return "", "", false
		}
		attribute := func(key string) string {
			if value, ok := parsed.MediaDescriptions[0].Attribute(key); ok {
				return value
			}
			value, _ := parsed.Attribute(key)
			return value
		}
		u, p := attribute("ice-ufrag"), attribute("ice-pwd")
		return u, p, u != "" && p != ""
	}
	u0, p0, ok0 := credentials(*previous)
	u1, p1, ok1 := credentials(next)
	return ok0 && ok1 && u0 != u1 && p0 != p1
}

func (p *Peer) restartReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	select {
	case <-p.connected:
	default:
		return ErrRestartState
	}
	if p.pc.SignalingState() != webrtc.SignalingStateStable || p.pendingRestart != 0 {
		return ErrRestartState
	}
	return nil
}

// CreateRestartOffer is only used by the designated offerer (Hub). No retry is
// automatic here: a higher-level recovery budget must own the attempt and its
// answer deadline. Failed gathering closes this peer rather than leaving an
// unanswerable local offer and silently continuing the cellular call.
func (p *Peer) CreateRestartOffer(parent context.Context) (RestartDescription, error) {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	return p.createRestartOffer(parent)
}

// Update TURN authentication before gathering a new allocation. The policy is
// immutable for this call; a failed issuer cannot downgrade relay-only media.
func (p *Peer) CreateRestartOfferWithICE(parent context.Context, credentials turncred.Credentials) (RestartDescription, error) {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if err := p.restartReady(parent); err != nil {
		return RestartDescription{}, err
	}
	if !credentials.Enabled || credentials.Validate(time.Now()) != nil || credentials.RelayOnly != p.config.RelayOnly {
		return RestartDescription{}, turncred.ErrConfig
	}
	config := p.pc.GetConfiguration()
	for _, old := range config.ICEServers {
		if old.Username == credentials.Servers[0].Username {
			return RestartDescription{}, turncred.ErrConfig
		}
	}
	config.ICEServers = nil
	for _, s := range credentials.Servers {
		config.ICEServers = append(config.ICEServers, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	if err := p.pc.SetConfiguration(config); err != nil {
		return RestartDescription{}, err
	}
	return p.createRestartOffer(parent)
}

func (p *Peer) createRestartOffer(parent context.Context) (RestartDescription, error) {
	if err := p.restartReady(parent); err != nil {
		return RestartDescription{}, err
	}
	if p.restartID == math.MaxUint64 {
		return RestartDescription{}, ErrRestartState
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	p.fullGatherOnly.Store(true)
	offer, err := p.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		p.fail(errors.New("ICE restart offer creation failed"))
		return RestartDescription{}, err
	}
	p.receiver.beginTransportRecovery(time.Now())
	p.restartID++
	p.pendingRestart = p.restartID
	p.pendingICE = nil
	description, err := p.gatherDescription(ctx, offer, false)
	if err != nil {
		p.fail(errors.New("ICE restart offer gathering failed"))
		return RestartDescription{}, err
	}
	return RestartDescription{ID: p.restartID, Description: description}, nil
}

func (p *Peer) AcceptRestartOffer(parent context.Context, offer RestartDescription) (RestartDescription, error) {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if err := p.restartReady(parent); err != nil {
		return RestartDescription{}, err
	}
	if p.restartID == math.MaxUint64 || offer.ID != p.restartID+1 {
		return RestartDescription{}, ErrSignalBinding
	}
	if !p.matches(offer.Description.CallUUID, offer.Description.Generation) {
		return RestartDescription{}, ErrSignalBinding
	}
	if !changedICE(p.pc.RemoteDescription(), offer.Description.SDP) {
		return RestartDescription{}, ErrSignalBinding
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	// Candidates belonging to the initial negotiation cannot enter a restart.
	p.fullGatherOnly.Store(true)
	p.pendingICE = nil
	if err := p.remoteDescription(offer.Description, webrtc.SDPTypeOffer); err != nil {
		p.fail(errors.New("ICE restart remote description failed"))
		return RestartDescription{}, err
	}
	p.receiver.beginTransportRecovery(time.Now())
	p.restartID = offer.ID
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		p.fail(errors.New("ICE restart answer failed"))
		return RestartDescription{}, err
	}
	description, err := p.gatherDescription(ctx, answer, false)
	if err != nil {
		p.fail(errors.New("ICE restart answer gathering failed"))
		return RestartDescription{}, err
	}
	return RestartDescription{ID: offer.ID, Description: description}, nil
}

func (p *Peer) AcceptRestartAnswer(answer RestartDescription) error {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.pendingRestart == 0 || answer.ID != p.pendingRestart {
		return ErrSignalBinding
	}
	if !changedICE(p.pc.RemoteDescription(), answer.Description.SDP) {
		return ErrSignalBinding
	}
	if err := p.remoteDescription(answer.Description, webrtc.SDPTypeAnswer); err != nil {
		return err
	}
	p.pendingRestart = 0
	return nil
}
