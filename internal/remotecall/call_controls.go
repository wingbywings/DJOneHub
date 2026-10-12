package remotecall

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
)

// A replay carries the same sequence and value. Mute changes do not advance
// business revisions, nor grant permission to activate a suspended call.
func (s *Session) SetMuted(installation, uuid string, generation, sequence uint64, muted bool) error {
	if !s.matches(installation, uuid, generation) {
		return ErrBinding
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.config.CallControls || s.ctx.Err() != nil || s.snapshot.State != "connected" {
		return ErrState
	}
	if sequence == 0 || sequence < s.muteSequence || (sequence == s.muteSequence && muted != s.muted) {
		return ErrRevision
	}
	if sequence == s.muteSequence {
		return nil
	}
	if err := s.media.(interface{ SetMuted(bool) error }).SetMuted(muted); err != nil {
		return err
	}
	s.muteSequence, s.muted = sequence, muted
	return nil
}

func (s *Session) dtmf(ctx context.Context, installation string, request Request) (callcontrol.Result, error) {
	if !callsignal.DTMFDigit(request.Digit) {
		return callcontrol.Result{}, ErrState
	}
	command := callcontrol.Command{RequestID: request.RequestID, CallUUID: request.CallUUID, Type: "dtmf", Digit: request.Digit, ExpectedRevision: request.ExpectedRevision, Actor: installation}
	return s.commands.Do(ctx, command, func(ctx context.Context) (callcontrol.Result, error) {
		s.mu.Lock()
		if !s.config.CallControls || s.ctx.Err() != nil || s.snapshot.State != "connected" || !s.audioActive || s.dtmfBusy || time.Since(s.lastDTMF) < 120*time.Millisecond {
			s.mu.Unlock()
			return callcontrol.Result{}, ErrState
		}
		if request.ExpectedRevision != s.snapshot.Revision {
			s.mu.Unlock()
			return callcontrol.Result{}, ErrRevision
		}
		s.dtmfBusy, s.lastDTMF = true, time.Now()
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.dtmfBusy = false; s.mu.Unlock() }()
		// Bounded hardware IO outside the session lock; cancellation still ends
		// the call promptly. Unknown outcomes are cached, never automatically retried.
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		stop := context.AfterFunc(s.ctx, cancel)
		defer func() { stop(); cancel() }()
		if s.ctx.Err() != nil {
			cancel()
		}
		err := s.backend.(interface {
			DTMF(context.Context, string) error
		}).DTMF(bounded, request.Digit)
		return callcontrol.Result{CallUUID: request.CallUUID, Type: "dtmf", Accepted: err == nil}, err
	})
}
