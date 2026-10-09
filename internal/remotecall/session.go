// Package remotecall coordinates a single authorized remote incoming call.
// Authentication and invitation delivery belong to the signaling adapter.
package remotecall

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

var (
	ErrBinding  = errors.New("remote call identity or installation mismatch")
	ErrRoute    = errors.New("call is not routed to this remote installation")
	ErrRevision = errors.New("stale remote session revision")
	ErrState    = errors.New("remote action is not valid in this state")
)

type Binding struct {
	CallUUID       string `json:"call_uuid"`
	Generation     uint64 `json:"generation"`
	InstallationID string `json:"installation_id"`
}

// Backend is already bound to the same call as the session. Every operation
// must recheck that binding at the actual hardware boundary. Finish must retain
// the audio lease if cellular teardown remains unconfirmed.
type Backend interface {
	Claim(context.Context) error
	Answer(context.Context) (mayHaveAnswered bool, err error)
	Reject(context.Context) error
	Observe() (Observation, error)
	Finish(context.Context, bool) error
}
type Observation struct{ CellularActive, ModuleReady, Ended bool }
type Media interface {
	TransportConnected() bool
	UpdateReadiness(remotemedia.Readiness) error
	AnswerReady() bool
	Activate() error
	Done() <-chan struct{}
	Close()
}

var _ Media = (*remotemedia.Peer)(nil)

type Config struct {
	Binding      Binding
	Route        callcontrol.RouteSnapshot
	RingDeadline time.Time
	Outgoing     bool
	// Zero selects production defaults. Shorter budgets are useful for tests;
	// these are internal policy, never copied from an untrusted request.
	AnswerTimeout, AudioTimeout, CleanupTimeout time.Duration
}
type Snapshot struct {
	Binding
	Revision     uint64 `json:"revision"`
	State        string `json:"state"`
	EndReason    string `json:"end_reason,omitempty"`
	CleanupError string `json:"cleanup_error,omitempty"`
}
type Request struct {
	CallUUID         string
	Generation       uint64
	RequestID        string
	ExpectedRevision uint64
	Type             string
}

// Session owns one worker and one media generation. Slow backend operations
// never hold mu. Cancellation stops preparation immediately; Done closes only
// after the worker reconciles the ATA outcome and attempts bounded cleanup.
type Session struct {
	mu             sync.Mutex
	config         Config
	backend        Backend
	media          Media
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	snapshot       Snapshot
	intent         string
	answerDeadline time.Time
	audioRevision  uint64
	audioActive    bool
	commands       callcontrol.Commands
}

func New(parent context.Context, config Config, backend Backend, factory func(context.Context) (Media, error)) (*Session, error) {
	b := config.Binding
	if b.CallUUID == "" || b.Generation == 0 || b.InstallationID == "" || backend == nil || factory == nil {
		return nil, ErrBinding
	}
	if config.Route.Mode != callcontrol.Remote || config.Route.TargetInstallationID != b.InstallationID {
		return nil, ErrRoute
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if !config.RingDeadline.After(time.Now()) {
		return nil, context.DeadlineExceeded
	}
	if config.AnswerTimeout == 0 {
		config.AnswerTimeout = 12 * time.Second
		if config.Outgoing {
			config.AnswerTimeout = 60 * time.Second
		}
	}
	if config.AudioTimeout == 0 {
		config.AudioTimeout = 3 * time.Second
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = 12 * time.Second
	}
	maxAnswer := 12 * time.Second
	if config.Outgoing {
		maxAnswer = 90 * time.Second
	}
	if config.AnswerTimeout <= 0 || config.AnswerTimeout > maxAnswer || config.AudioTimeout <= 0 || config.AudioTimeout > 3*time.Second || config.CleanupTimeout <= 0 || config.CleanupTimeout > 12*time.Second {
		return nil, ErrState
	}
	ctx, cancel := context.WithCancel(parent)
	media, err := factory(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if media == nil {
		cancel()
		return nil, ErrState
	}
	s := &Session{config: config, backend: backend, media: media, ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), snapshot: Snapshot{Binding: b, Revision: 1, State: "offered"}}
	go s.run()
	go func() {
		select {
		case <-media.Done():
			s.end("media_failed")
		case <-ctx.Done():
		}
	}()
	return s, nil
}
func (s *Session) Snapshot() Snapshot    { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot }
func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Cancel()               { s.end("cancelled") }
func (s *Session) AnsweredElsewhere()    { s.end("answered_elsewhere") }
func (s *Session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Session) matches(installation, uuid string, generation uint64) bool {
	b := s.config.Binding
	return installation == b.InstallationID && uuid == b.CallUUID && generation == b.Generation
}

// installation is supplied by an authenticated connection, not by its JSON.
// This command acknowledges intent; answer-ready and connected are separate
// observable transitions, never inferred from successful ATA submission.
func (s *Session) Command(ctx context.Context, installation string, request Request) (callcontrol.Result, error) {
	if !s.matches(installation, request.CallUUID, request.Generation) {
		return callcontrol.Result{}, ErrBinding
	}
	if request.RequestID == "" || len(request.RequestID) > 128 || request.ExpectedRevision == 0 {
		return callcontrol.Result{}, ErrRevision
	}
	if request.Type != "answer" && request.Type != "reject" && request.Type != "hangup" {
		return callcontrol.Result{}, ErrState
	}
	command := callcontrol.Command{RequestID: request.RequestID, CallUUID: request.CallUUID, Type: request.Type, ExpectedRevision: request.ExpectedRevision, Actor: installation}
	return s.commands.Do(ctx, command, func(ctx context.Context) (callcontrol.Result, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return callcontrol.Result{}, err
		}
		if err := s.ctx.Err(); err != nil {
			return callcontrol.Result{}, err
		}
		if request.ExpectedRevision != s.snapshot.Revision {
			return callcontrol.Result{}, ErrRevision
		}
		if request.Type == "answer" || request.Type == "reject" {
			if s.snapshot.State != "offered" || !time.Now().Before(s.config.RingDeadline) {
				return callcontrol.Result{}, ErrState
			}
			s.intent = request.Type
			s.answerDeadline = time.Now().Add(s.config.AnswerTimeout)
			s.snapshot.State = "answering"
			if request.Type == "reject" {
				s.snapshot.State = "rejecting"
			}
			s.snapshot.Revision++
			s.signal()
		} else {
			if s.snapshot.State == "rejecting" || (s.snapshot.State == "offered" && !s.config.Outgoing) {
				return callcontrol.Result{}, ErrState
			}
			s.endLocked("remote_hangup")
		}
		return callcontrol.Result{CallUUID: request.CallUUID, Type: request.Type, Accepted: true}, nil
	})
}

// AudioActive uses its own monotonic sequence, independent of business state.
// Deactivation ends the first-version session; interruption recovery is future
// work. Stale activation cannot reverse a later deactivation or terminal state.
func (s *Session) AudioActive(installation, uuid string, generation, revision uint64, active bool) error {
	if !s.matches(installation, uuid, generation) {
		return ErrBinding
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.snapshot.State != "awaiting_audio" && s.snapshot.State != "connected" {
		return ErrState
	}
	if revision == 0 || revision < s.audioRevision {
		return ErrRevision
	}
	if revision == s.audioRevision {
		if active != s.audioActive {
			return ErrRevision
		}
		return nil
	}
	s.audioRevision, s.audioActive = revision, active
	if !active {
		s.endLocked("audio_deactivated")
	} else {
		s.signal()
	}
	return nil
}
func (s *Session) endLocked(reason string) {
	if s.snapshot.State != "ended" {
		s.snapshot.State, s.snapshot.EndReason = "ended", reason
		s.snapshot.Revision++
	}
	s.cancel()
}
func (s *Session) end(reason string) {
	if reason == "cancelled" || reason == "call_unavailable" || reason == "answer_timeout" || reason == "answer_failed" || reason == "media_failed" {
		if observed, err := s.backend.Observe(); err == nil && observed.Ended {
			reason = "cellular_ended"
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(reason)
}
func (s *Session) transition(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.snapshot.State == "ended" {
		return false
	}
	s.snapshot.State = state
	s.snapshot.Revision++
	return true
}
func (s *Session) wait(ctx context.Context) error {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.media.Done():
		return errors.New("remote media ended")
	case <-s.wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (s *Session) run() {
	claimed, mayHaveAnswered := false, false
	stopClose := context.AfterFunc(s.ctx, s.media.Close)
	defer func() {
		s.end("cancelled")
		stopClose()
		s.media.Close()
		if claimed {
			ctx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
			err := s.backend.Finish(ctx, mayHaveAnswered)
			cancel()
			if err != nil {
				s.mu.Lock()
				s.snapshot.CleanupError = err.Error()
				s.snapshot.Revision++
				s.mu.Unlock()
			}
		}
		close(s.done)
	}()
	ringCtx, stopRing := context.WithDeadline(s.ctx, s.config.RingDeadline)
	defer stopRing()
	var intent string
	var deadline time.Time
	for {
		s.mu.Lock()
		intent, deadline = s.intent, s.answerDeadline
		s.mu.Unlock()
		if intent != "" {
			break
		}
		if err := s.wait(ringCtx); err != nil {
			reason := "media_failed"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "ring_timeout"
			}
			if s.ctx.Err() != nil {
				reason = "cancelled"
			}
			s.end(reason)
			return
		}
	}
	answerCtx, stopAnswer := context.WithDeadline(s.ctx, deadline)
	defer stopAnswer()
	if err := s.backend.Claim(answerCtx); err != nil {
		s.end("claim_failed")
		return
	}
	claimed = true
	if intent == "reject" {
		if err := s.backend.Reject(answerCtx); err != nil {
			s.end("reject_failed")
		} else {
			s.end("rejected")
		}
		return
	}
	for !s.media.TransportConnected() {
		if err := s.wait(answerCtx); err != nil {
			s.end("transport_unavailable")
			return
		}
	}
	var err error
	mayHaveAnswered, err = s.backend.Answer(answerCtx)
	if err != nil {
		s.end("answer_failed")
		return
	}
	var previous remotemedia.Readiness
	update := func() error {
		observed, err := s.backend.Observe()
		if err != nil {
			return err
		}
		if observed.Ended {
			return errors.New("cellular call ended")
		}
		s.mu.Lock()
		audio := s.audioActive
		s.mu.Unlock()
		state := remotemedia.Readiness{CallUUID: s.config.Binding.CallUUID, Generation: s.config.Binding.Generation, Revision: previous.Revision, CellularActive: observed.CellularActive, ModuleReady: observed.ModuleReady, IOSAudioActive: audio}
		if previous.Revision == 0 || state != previous {
			state.Revision++
			if err := s.media.UpdateReadiness(state); err != nil {
				return err
			}
			previous = state
		}
		return nil
	}
	for {
		if err := answerCtx.Err(); err != nil {
			s.end("answer_timeout")
			return
		}
		if err := update(); err != nil {
			s.end("call_unavailable")
			return
		}
		if s.media.AnswerReady() {
			break
		}
		if err := s.wait(answerCtx); err != nil {
			s.end("answer_timeout")
			return
		}
	}
	if !s.transition("awaiting_audio") {
		return
	}
	audioCtx, stopAudio := context.WithTimeout(s.ctx, s.config.AudioTimeout)
	defer stopAudio()
	for {
		if err := audioCtx.Err(); err != nil {
			s.end("audio_timeout")
			return
		}
		if err := update(); err != nil {
			s.end("call_unavailable")
			return
		}
		if previous.IOSAudioActive && s.media.AnswerReady() {
			if err := s.media.Activate(); err != nil {
				s.end("media_failed")
				return
			}
			break
		}
		if err := s.wait(audioCtx); err != nil {
			s.end("audio_timeout")
			return
		}
	}
	if !s.transition("connected") {
		return
	}
	for {
		if err := s.wait(s.ctx); err != nil {
			s.end("media_failed")
			return
		}
		if err := update(); err != nil {
			s.end("call_unavailable")
			return
		}
		if !s.media.AnswerReady() {
			s.end("media_unavailable")
			return
		}
	}
}
