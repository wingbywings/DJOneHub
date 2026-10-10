package remotemedia

import (
	"errors"
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
}

// Receiver owns one decoder and one RTP stream. Push and Read may run on
// different goroutines; decode is serialized. Read emits one 10 ms PCM frame
// per actual clock tick. Startup/rebuffer delay is 60 ms, with a 200 ms window.
// Adaptive delay and long-term sample-clock drift correction are separate work.
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
}

func NewReceiver(generation uint64, decoder VoiceDecoder, samples func([]byte) (int, error)) (*Receiver, error) {
	if generation == 0 || decoder == nil || samples == nil {
		return nil, ErrFrame
	}
	return &Receiver{generation: generation, decoder: decoder, packetSamples: samples, packets: make(map[int64]receivedPacket), seen: make(map[int64]struct{})}, nil
}
func (r *Receiver) Push(packet IncomingRTP, now time.Time) error {
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
		if seq < r.maxSeq-128 || seq > r.maxSeq+512 {
			return reject(ErrRTPWindow)
		}
		if _, ok := r.seen[seq]; ok {
			r.stats.Duplicates++
			return ErrRTPDuplicate
		}
	}
	if r.started {
		if ts < r.cursor && r.played {
			r.stats.LatePackets++
			return ErrRTPWindow
		}
		if ts < r.cursor-5760 || ts > r.cursor+9600 || (ts-r.origin)%120 != 0 {
			return reject(ErrRTPWindow)
		}
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
	if !r.started {
		r.started = true
		r.played = false
		r.cursor = ts
		r.origin = ts
		r.deadline = now.Add(60 * time.Millisecond)
	} else if !r.played && ts < r.cursor {
		r.cursor = ts
	}
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
	r.packets[ts] = receivedPacket{arrived: now, sequence: seq, timestamp: ts, samples: samples, payload: append([]byte(nil), packet.Payload...)}
	r.stats.Packets++
	return nil
}

// Reset clears buffered audio while retaining the SSRC and replay window. A
// replacement stream needs a new Receiver/media generation.
func (r *Receiver) Reset() error { r.mu.Lock(); defer r.mu.Unlock(); return r.resetLocked() }
func (r *Receiver) resetLocked() error {
	clear(r.packets)
	r.pcm = nil
	r.started = false
	r.played = false
	r.concealed = 0
	r.haveDecoded = false
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
			r.deadline = now.Add(60 * time.Millisecond)
			r.started = true
		}
		return nil, nil
	}
	r.played = true
	for len(r.pcm) < FrameSamples {
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
	copy(frame.PCM[:], r.pcm[:FrameSamples])
	r.pcm = r.pcm[FrameSamples:]
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
	return stats
}
