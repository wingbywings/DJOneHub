package remotemedia

// SetMuted drops iPhone uplink at the Hub in addition to the phone's local
// microphone gate. Downlink and call activation are independent of mute.
func (p *Peer) SetMuted(muted bool) error {
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	if p.ctx.Err() != nil || p.quiesced.Load() {
		return ErrMediaNotReady
	}
	if p.muted == muted {
		return nil
	}
	p.muted = muted
	// Keep generation, SSRC and replay history across both transitions.
	if err := p.receiver.Reset(); err != nil {
		return err
	}
	p.bridge.discardPlayback()
	return nil
}

func (b *HostBridge) discardPlayback() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for n := 0; n < cap(b.outgoing); n++ {
		select {
		case <-b.outgoing:
		default:
		}
	}
}
