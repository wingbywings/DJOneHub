package remotemedia

import (
	"errors"
	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
	"time"
)

const ControlMessageLimit = 8192

var ErrControlUnavailable = errors.New("call control channel unavailable")

func (p *Peer) openControl() error {
	ordered, protocol := true, "djonehub.calls.v1"
	dc, err := p.pc.CreateDataChannel("call-control", &webrtc.DataChannelInit{Ordered: &ordered, Protocol: &protocol})
	if err != nil {
		return err
	}
	p.control = dc
	limiter := rate.NewLimiter(10, 20)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if !m.IsString || len(m.Data) == 0 || len(m.Data) > ControlMessageLimit || !limiter.Allow() {
			p.fail(errors.New("invalid call control frame"))
			return
		}
		select {
		case p.controlMessages <- append([]byte(nil), m.Data...):
		case <-p.ctx.Done():
		default:
			p.fail(errors.New("call control backlog exhausted"))
		}
	})
	p.pc.OnDataChannel(func(other *webrtc.DataChannel) { _ = other.Close(); p.fail(errors.New("unexpected control channel")) })
	return nil
}
func (p *Peer) ControlMessages() <-chan []byte { return p.controlMessages }
func (p *Peer) ControlReady() bool {
	return p.control != nil && p.ctx.Err() == nil && p.control.ReadyState() == webrtc.DataChannelStateOpen
}
func (p *Peer) SendControl(data []byte) error {
	if !p.ControlReady() || len(data) == 0 || len(data) > ControlMessageLimit || p.control.BufferedAmount() > 32768 {
		return ErrControlUnavailable
	}
	return p.control.SendText(string(data))
}

// Quiesce immediately stops both audio directions and releases the host. The
// already authenticated DTLS/SCTP association may deliver the cleanup result.
// It cannot reactivate audio or acquire another call's lease.
func (p *Peer) Quiesce() {
	p.quiesced.Store(true)
	p.mediaMu.Lock()
	p.active = false
	p.readiness.Ended = true
	p.mediaMu.Unlock()
	p.bridge.Close()
}
func (p *Peer) retainControlUntilCleanup(parentDone <-chan struct{}) {
	select {
	case <-parentDone:
	case <-p.ctx.Done():
		return
	}
	p.Quiesce()
	timer := time.NewTimer(15 * time.Second) // cleanup <=12s + bounded terminal receipt
	defer timer.Stop()
	select {
	case <-timer.C:
		p.Close()
	case <-p.ctx.Done():
	}
}
