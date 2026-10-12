package remotemedia

import (
	"errors"
	"math"
	"sync"
	"time"
)

var ErrRTPStream = errors.New("RTP stream or media generation mismatch")
var ErrRTPWindow = errors.New("RTP packet outside playout window")
var ErrRTPDuplicate = errors.New("duplicate or overlapping RTP packet")

type VoiceDecoder interface {
	Decode([]byte) ([]int16, error)
	Recover([]byte, int) ([]int16, error)
	Conceal(int) ([]int16, error)
	ResetDecoder() error
}
type IncomingRTP struct {
	Generation uint64
	SSRC       uint32
	Sequence   uint16
	Timestamp  uint32
	Payload    []byte
}
type receivedPacket struct {
	arrived             time.Time
	sequence, timestamp int64
	samples             int
	payload             []byte
}
type ReceiverStats struct {
	Packets, DecodedPackets, LatePackets, Duplicates, RejectedPackets, FECRequests, PLCSamples, SilenceSamples, Rebuffers uint64
	BufferedPackets                                                                                                       int
	TargetDelayMS, JitterMS                                                                                               float64
	LateRecoveries                                                                                                        uint64
	ClockCorrectionPPM                                                                                                    float64
}

// Receiver owns one decoder and one RTP stream. Push and Read may run on
// different goroutines; decode is serialized. Read emits one 10 ms PCM frame
// per actual clock tick. Startup is 60 ms; delay adapts within 40–120 ms.
// Sustained late input can rebuffer without replaying already decoded speech.
type Receiver struct {
	haveDecoded             bool
	lastDecodedSeq          int64
	mu                      sync.Mutex
	generation              uint64
	decoder                 VoiceDecoder
	packetSamples           func([]byte) (int, error)
	pinned, started, played bool
	ssrc                    uint32
	maxSeq, maxTimestamp    int64
	origin, cursor          int64
	deadline                time.Time
	packets                 map[int64]receivedPacket
	seen                    map[int64]struct{}
	pcm                     []int16
	sequence, sampleIndex   uint64
	concealed               int
	stats                   ReceiverStats
	targetDelay             time.Duration
	jitter                  float64
	transitAt               time.Time
	transitTimestamp        int64
	stableSince             time.Time
	lateRun                 int
	decodedUntil            int64
	decodedAny              bool
	clock                   sampleClock
	recoveryUntil           time.Time
}

func NewReceiver(generation uint64, decoder VoiceDecoder, samples func([]byte) (int, error)) (*Receiver, error) {
	if generation == 0 || decoder == nil || samples == nil {
		return nil, ErrFrame
	}
	return &Receiver{generation: generation, decoder: decoder, packetSamples: samples, targetDelay: 60 * time.Millisecond, packets: make(map[int64]receivedPacket), seen: make(map[int64]struct{})}, nil
}
func (r *Receiver) Push(packet IncomingRTP, now time.Time) error { return r.accept(packet, now, false) }

// Discard authenticates stream/replay bounds and advances the receive window
// without retaining payload or decoding. Long mute periods cannot freeze the
// sequence window and permanently silence a subsequent unmute.
func (r *Receiver) Discard(packet IncomingRTP, now time.Time) error {
	return r.accept(packet, now, true)
}

func (r *Receiver) accept(packet IncomingRTP, now time.Time, discard bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reject := func(err error) error { r.stats.RejectedPackets++; return err }
	if packet.Generation != r.generation || packet.SSRC == 0 || (r.pinned && packet.SSRC != r.ssrc) {
		return reject(ErrRTPStream)
	}
	if len(packet.Payload) == 0 || len(packet.Payload) > 1275 {
		return reject(ErrFrame)
	}
	samples, err := r.packetSamples(packet.Payload)
	if err != nil || samples <= 0 || samples > 960 || samples%20 != 0 {
		return reject(ErrFrame)
	}
	seq, ts := int64(packet.Sequence), int64(packet.Timestamp)
	if r.pinned {
		seq = r.maxSeq + int64(int16(packet.Sequence-uint16(r.maxSeq)))
		ts = r.maxTimestamp + int64(int32(packet.Timestamp-uint32(r.maxTimestamp)))
		if seq < r.maxSeq-128 {
			return reject(ErrRTPWindow)
		}
		if seq > r.maxSeq+512 {
			// Only an authenticated ICE renegotiation opens this one-shot gap
			// allowance. Preserve SSRC, generation and decoded replay bounds.
			elapsed := now.Sub(r.transitAt)
			if r.recoveryUntil.IsZero() || !now.Before(r.recoveryUntil) ||
				elapsed < time.Second || elapsed > 20*time.Second ||
				seq > r.maxSeq+8192 || ts <= r.maxTimestamp ||
				ts-r.maxTimestamp > int64((elapsed+250*time.Millisecond).Seconds()*48000) ||
				(r.decodedAny && ts < r.decodedUntil) ||
				(r.started && (ts-r.origin)%120 != 0) {
				return reject(ErrRTPWindow)
			}
			if err := r.resetLocked(); err != nil {
				return err
			}
			r.recoveryUntil = time.Time{}
			r.stats.Rebuffers++
		}
		if _, ok := r.seen[seq]; ok {
			r.stats.Duplicates++
			return ErrRTPDuplicate
		}
	}
	if r.started && (ts-r.origin)%120 != 0 {
		return reject(ErrRTPWindow)
	}
	// A decoded timestamp must never be replayed, even after rebuffer/reset.
	if r.decodedAny && ts < r.decodedUntil {
		r.stats.LatePackets++
		return ErrRTPWindow
	}
	if r.started && r.played && ts < r.cursor {
		r.stats.LatePackets++
		// Advance the replay window on fresh late packets too. Otherwise 512
		// rejected packets would permanently lock out the remainder of a call.
		if seq > r.maxSeq && ts > r.maxTimestamp {
			r.observeArrival(ts, now)
			r.remember(seq, ts)
			r.lateRun++
			if r.lateRun >= 3 {
				r.targetDelay = min(120*time.Millisecond, r.targetDelay+20*time.Millisecond)
				if err := r.resetLocked(); err != nil {
					return err
				}
				r.stats.Rebuffers++
				r.stats.LateRecoveries++
			} else {
				return ErrRTPWindow
			}
		} else {
			return ErrRTPWindow
		}
	}
	if r.started && (ts < r.cursor-5760 || ts > r.cursor+9600) {
		return reject(ErrRTPWindow)
	}

	for _, queued := range r.packets {
		if ts < queued.timestamp+int64(queued.samples*6) && ts+int64(samples*6) > queued.timestamp {
			r.stats.Duplicates++
			return ErrRTPDuplicate
		}
	}
	if len(r.packets) >= 64 {
		return reject(ErrRTPWindow)
	}
	if !r.pinned {
		r.pinned = true
		r.ssrc = packet.SSRC
		r.maxSeq = seq
		r.maxTimestamp = ts
	}
	if discard {
		if r.transitAt.IsZero() || seq > r.maxSeq {
			r.observeArrival(ts, now)
		}
		r.remember(seq, ts)
		r.decodedUntil = max(r.decodedUntil, ts+int64(samples*6))
		r.decodedAny = true
		r.stats.Packets++
		return nil
	}
	if !r.started {
		r.started = true
		r.played = false
		r.cursor = ts
		r.origin = ts
		r.deadline = now.Add(r.targetDelay)
	} else if !r.played && ts < r.cursor {
		r.cursor = ts
	}
	if r.transitAt.IsZero() || seq > r.maxSeq {
		r.observeArrival(ts, now)
	}
	r.remember(seq, ts)
	r.lateRun = 0

	r.packets[ts] = receivedPacket{arrived: now, sequence: seq, timestamp: ts, samples: samples, payload: append([]byte(nil), packet.Payload...)}
	r.stats.Packets++
	return nil
}

// Reset clears buffered audio while retaining the SSRC and replay window. A
// replacement stream needs a new Receiver/media generation.
// beginTransportRecovery is called only by the validated ICE restart path.
// It does not clear any replay state or accept a replacement stream.
func (r *Receiver) beginTransportRecovery(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recoveryUntil = now.Add(20 * time.Second)
}

func (r *Receiver) Reset() error { r.mu.Lock(); defer r.mu.Unlock(); return r.resetLocked() }
func (r *Receiver) resetLocked() error {
	clear(r.packets)
	r.pcm = nil
	r.started = false
	r.played = false
	r.concealed = 0
	r.haveDecoded = false
	r.lateRun = 0
	r.clock = sampleClock{}
	return r.decoder.ResetDecoder()
}
func (r *Receiver) Read(now time.Time) (*Frame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || now.Before(r.deadline) {
		return nil, nil
	}
	if now.Sub(r.deadline) > 40*time.Millisecond {
		r.sampleIndex += uint64(now.Sub(r.deadline)/(10*time.Millisecond)) * FrameSamples
		// Keep only the newest packet, then rebuild decoder state. Old decoded
		// audio and pre-stall packets must not play after a scheduler suspension.
		var latest *receivedPacket
		for _, packet := range r.packets {
			if now.Sub(packet.arrived) <= 60*time.Millisecond && (latest == nil || packet.timestamp > latest.timestamp) {
				copy := packet
				latest = &copy
			}
		}
		if err := r.resetLocked(); err != nil {
			return nil, err
		}
		r.stats.Rebuffers++
		if latest != nil {
			r.packets[latest.timestamp] = *latest
			r.cursor = latest.timestamp
			r.origin = latest.timestamp
			r.deadline = now.Add(r.targetDelay)
			r.started = true
		}
		return nil, nil
	}
	r.played = true
	horizon := r.cursor
	for _, packet := range r.packets {
		horizon = max(horizon, packet.timestamp+int64(packet.samples*6))
	}
	if len(r.packets) > 0 {
		occupancy := float64(horizon-r.cursor)/6 + float64(len(r.pcm))
		r.clock.update(occupancy-r.targetDelay.Seconds()*8000-120, now)
	}
	needed := r.clock.needed()
	for len(r.pcm) < needed {
		if packet, ok := r.packets[r.cursor]; ok {
			delete(r.packets, r.cursor)
			pcm, err := r.decoder.Decode(packet.payload)
			if err != nil {
				return nil, err
			}
			if len(pcm) != packet.samples {
				return nil, ErrFrame
			}
			r.pcm = append(r.pcm, pcm...)
			r.cursor += int64(packet.samples * 6)
			r.concealed = 0
			r.stats.DecodedPackets++
			r.decodedAny = true
			r.decodedUntil = r.cursor
			r.haveDecoded = true
			r.lastDecodedSeq = packet.sequence
			continue
		}
		var next *receivedPacket
		for _, packet := range r.packets {
			if packet.timestamp > r.cursor && (next == nil || packet.timestamp < next.timestamp) {
				copy := packet
				next = &copy
			}
		}
		if next != nil && next.timestamp-r.cursor == 960 && next.samples == 160 && r.concealed == 0 && r.haveDecoded && next.sequence == r.lastDecodedSeq+2 {
			pcm, err := r.decoder.Recover(next.payload, 160)
			if err != nil {
				return nil, err
			}
			if len(pcm) != 160 {
				return nil, ErrFrame
			}
			r.pcm = append(r.pcm, pcm...)
			r.cursor += 960
			r.decodedAny, r.decodedUntil = true, r.cursor
			r.concealed += 160
			r.stats.FECRequests++
			continue
		}
		missing := FrameSamples
		if next != nil && next.timestamp-r.cursor < int64(missing*6) {
			missing = int((next.timestamp - r.cursor) / 6)
		}
		if missing <= 0 || missing%20 != 0 {
			return nil, ErrFrame
		}
		var pcm []int16
		if r.concealed+missing <= 480 {
			var err error
			pcm, err = r.decoder.Conceal(missing)
			if err != nil {
				return nil, err
			}
			if len(pcm) != missing {
				return nil, ErrFrame
			}
			r.stats.PLCSamples += uint64(missing)
		} else {
			if r.concealed == 480 {
				if err := r.decoder.ResetDecoder(); err != nil {
					return nil, err
				}
			}
			pcm = make([]int16, missing)
			r.stats.SilenceSamples += uint64(missing)
		}
		r.concealed += missing
		r.pcm = append(r.pcm, pcm...)
		r.cursor += int64(missing * 6)
	}
	frame := &Frame{Direction: ToModem, Generation: r.generation, Sequence: r.sequence, SampleIndex: r.sampleIndex}
	consumed := r.clock.render(r.pcm, frame.PCM[:])
	r.pcm = r.pcm[consumed:]
	r.sequence++
	r.sampleIndex += FrameSamples
	// Advance the media clock, not the scheduler wakeup time. Otherwise tiny
	// ticker phase differences skip render turns until valid incoming RTP is
	// permanently ahead of the admission window. Real stalls rebuffer above.
	r.deadline = r.deadline.Add(10 * time.Millisecond)
	return frame, nil
}
func (r *Receiver) Stats() ReceiverStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := r.stats
	stats.BufferedPackets = len(r.packets)
	stats.TargetDelayMS = float64(r.targetDelay) / float64(time.Millisecond)
	stats.JitterMS = r.jitter / 48
	stats.ClockCorrectionPPM = r.clock.ppm
	return stats
}

// RFC 3550 interarrival variation, measured using a monotonic local clock.
// Only fresh, in-order timestamps update this estimator.
func (r *Receiver) observeArrival(ts int64, now time.Time) {
	if !r.transitAt.IsZero() && now.After(r.transitAt) {
		delta := float64(now.Sub(r.transitAt))*48000/float64(time.Second) - float64(ts-r.transitTimestamp)
		r.jitter += (math.Abs(delta) - r.jitter) / 16
		desired := time.Duration(math.Ceil((40+4*r.jitter/48)/10)) * 10 * time.Millisecond
		desired = max(40*time.Millisecond, min(120*time.Millisecond, desired))
		if desired > r.targetDelay {
			if r.started {
				r.deadline = r.deadline.Add(desired - r.targetDelay)
			}
			r.targetDelay = desired
			r.stableSince = now
		} else if desired < r.targetDelay {
			if r.stableSince.IsZero() {
				r.stableSince = now
			}
			// Lower the target conservatively; a future rebuffer applies it.
			// Never throw away speech solely to reduce the target delay.
			if now.Sub(r.stableSince) >= 20*time.Second {
				r.targetDelay -= 10 * time.Millisecond
				r.stableSince = now
			}
		} else {
			r.stableSince = now
		}
	}
	r.transitAt, r.transitTimestamp = now, ts
}
func (r *Receiver) remember(seq, ts int64) {
	if seq > r.maxSeq {
		r.maxSeq = seq
	}
	if ts > r.maxTimestamp {
		r.maxTimestamp = ts
	}
	for old := range r.seen {
		if old < r.maxSeq-128 {
			delete(r.seen, old)
		}
	}
	r.seen[seq] = struct{}{}
}
