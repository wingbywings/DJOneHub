package remotemedia

import "errors"

var ErrReadinessRevision = errors.New("stale or conflicting media readiness revision")

// SuspendAudio preserves the authenticated transport/host lease, but no queued
// speech. Only a later trusted readiness update and Activate can resume media.
func (p *Peer) SuspendAudio() error {
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	if p.ctx.Err() != nil || p.quiesced.Load() {
		return ErrMediaNotReady
	}
	p.active = false
	p.audioSuspendRevision = p.readiness.Revision
	p.audioSuspendSequence = p.readiness.AudioSequence
	p.sender.Discard()
	p.bridge.DiscardQueuedAudio()
	return p.receiver.Reset()
}

// Readiness is a trusted coordinator snapshot, not a client request body.
// CellularActive/ModuleReady must come from the Hub; IOSAudioActive must come
// from the authorized installation's CallKit didActivate confirmation. The
// caller authenticates these facts before building this complete snapshot.
// Its revision is separate from call command revisions and never regresses.
type Readiness struct {
	CallUUID                                           string
	Generation, Revision                               uint64
	AudioSequence                                      uint64
	CellularActive, ModuleReady, IOSAudioActive, Ended bool
}

func (p *Peer) UpdateReadiness(state Readiness) error {
	if !p.matches(state.CallUUID, state.Generation) {
		return ErrSignalBinding
	}
	if state.Revision == 0 {
		return ErrReadinessRevision
	}
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.quiesced.Load() {
		return ErrMediaNotReady
	}
	if state.Revision < p.readiness.Revision {
		return ErrReadinessRevision
	}
	if state.Revision == p.readiness.Revision {
		if state == p.readiness {
			return nil
		}
		return ErrReadinessRevision
	}
	p.readiness = state
	if state.Ended {
		p.active = false
		p.cancel()
		return nil
	}
	if !state.CellularActive || !state.ModuleReady || !state.IOSAudioActive {
		p.active = false
		p.sender.Discard()
		return p.receiver.Reset()
	}
	return nil
}

// AnswerReady deliberately excludes iOS audio activation. CallKit must first
// fulfill the answer action, then report didActivate; requiring it here would
// deadlock the iOS audio session startup. Actual PCM still requires Activate.
func (p *Peer) AnswerReady() bool {
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	return p.ctx.Err() == nil && !p.readiness.Ended && p.readiness.CellularActive && p.readiness.ModuleReady && p.transportReady()
}
