package remotemedia

import (
	"errors"
	"sync"
	"time"
)

// VoiceEncoder is owned by Sender. Close it after stopping its single consumer.
type VoiceEncoder interface{ Encode([]int16) ([]byte, error) }
type EncodedAudio struct {
	Payload     []byte
	SampleIndex uint64
	// WebRTC's Opus RTP clock is 48 kHz even when the PCM input is 8 kHz.
	Timestamp uint32
	Samples   int
}
type queuedPCM struct {
	frame   Frame
	arrived time.Time
}
type SenderStats struct {
	CapturedFrames                uint64
	SignalFrames                  uint64
	PeakPCM16                     uint32
	DroppedFrames, EncodedPackets uint64
	QueuedFrames                  int
}

// Sender retains at most 60 ms of capture audio, discards expired input, and
// emits at most one 20 ms packet per tick. It never catches up by burst sending.
// Push may run concurrently with Next. Exactly one goroutine calls Next.
type Sender struct {
	mu      sync.Mutex
	encoder VoiceEncoder
	guard   FrameGuard
	queue   []queuedPCM
	next    time.Time
	stats   SenderStats
}

func NewSender(generation uint64, encoder VoiceEncoder) (*Sender, error) {
	if generation == 0 || encoder == nil {
		return nil, errors.New("media generation and encoder are required")
	}
	return &Sender{encoder: encoder, guard: FrameGuard{Generation: generation, Direction: FromModem}}, nil
}
func (s *Sender) Push(f Frame, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard.Accept(f); err != nil {
		return err
	}
	s.stats.CapturedFrames++
	var peak uint32
	for _, sample := range f.PCM {
		value := int32(sample)
		if value < 0 {
			value = -value
		}
		if uint32(value) > peak {
			peak = uint32(value)
		}
	}
	// Amplitude counters only: never retain or expose captured audio. A small
	// threshold separates digital silence/noise from a useful input signal.
	if peak >= 128 {
		s.stats.SignalFrames++
	}
	if peak > s.stats.PeakPCM16 {
		s.stats.PeakPCM16 = peak
	}
	s.queue = append(s.queue, queuedPCM{f, now})
	if len(s.queue) > 6 {
		s.stats.DroppedFrames += uint64(len(s.queue) - 6)
		s.queue = append([]queuedPCM(nil), s.queue[len(s.queue)-6:]...)
	}
	return nil
}
func (s *Sender) Next(now time.Time) (*EncodedAudio, error) {
	s.mu.Lock()
	if now.Before(s.next) {
		s.mu.Unlock()
		return nil, nil
	}
	for len(s.queue) > 0 && now.Sub(s.queue[0].arrived) > 60*time.Millisecond {
		s.queue = s.queue[1:]
		s.stats.DroppedFrames++
	}
	for len(s.queue) >= 2 && s.queue[1].frame.SampleIndex != s.queue[0].frame.SampleIndex+FrameSamples {
		s.queue = s.queue[1:]
		s.stats.DroppedFrames++
	}
	if len(s.queue) < 2 {
		s.mu.Unlock()
		return nil, nil
	}
	first, second := s.queue[0].frame, s.queue[1].frame
	s.queue = s.queue[2:]
	// Keep the original cadence across ordinary timer jitter. Re-basing on
	// every wakeup makes a tick a few nanoseconds early miss an entire 10 ms
	// turn, repeatedly reducing the send rate. A genuine stall starts a fresh
	// cadence instead of catching up with a burst of old audio.
	if s.next.IsZero() || now.Sub(s.next) >= 20*time.Millisecond {
		s.next = now.Add(20 * time.Millisecond)
	} else {
		s.next = s.next.Add(20 * time.Millisecond)
	}
	s.mu.Unlock()
	pcm := make([]int16, 2*FrameSamples)
	copy(pcm, first.PCM[:])
	copy(pcm[FrameSamples:], second.PCM[:])
	encoded, err := s.encoder.Encode(pcm)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.stats.EncodedPackets++
	s.mu.Unlock()
	return &EncodedAudio{Payload: encoded, SampleIndex: first.SampleIndex, Timestamp: uint32(first.SampleIndex * 6), Samples: len(pcm)}, nil
}
func (s *Sender) Stats() SenderStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := s.stats
	stats.QueuedFrames = len(s.queue)
	return stats
}

// Discard removes captured audio when media readiness is revoked. Keep the
// replay guard and sample clock; reactivation must not replay an old capture.
func (s *Sender) Discard() { s.mu.Lock(); defer s.mu.Unlock(); s.queue = nil }
