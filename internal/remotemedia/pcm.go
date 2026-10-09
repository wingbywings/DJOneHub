// Package remotemedia implements the real-time media path independently of AI.
package remotemedia

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	SampleRate        = 8000
	FrameSamples      = 80 // 10 ms; exactly one frame per local WebSocket message.
	HeaderSize        = 32
	FrameSize         = HeaderSize + FrameSamples*2
	FromModem    byte = 1
	ToModem      byte = 2
)

var ErrFrame = errors.New("invalid remote PCM frame")
var ErrStale = errors.New("stale or overlapping remote PCM frame")

// Integers use little endian. Byte 4 is version 1; byte 5 is direction;
// bytes 6..7 are sample count; generation/sequence/sample index are uint64.
// A connection handshake separately binds the generation to its call UUID.
type Frame struct {
	Direction                         byte
	Generation, Sequence, SampleIndex uint64
	PCM                               [FrameSamples]int16
}

func (f Frame) MarshalBinary() ([]byte, error) {
	if (f.Direction != FromModem && f.Direction != ToModem) || f.Generation == 0 || f.SampleIndex > math.MaxUint64-FrameSamples {
		return nil, ErrFrame
	}
	data := make([]byte, FrameSize)
	copy(data, "DJRM")
	data[4] = 1
	data[5] = f.Direction
	binary.LittleEndian.PutUint16(data[6:], FrameSamples)
	binary.LittleEndian.PutUint64(data[8:], f.Generation)
	binary.LittleEndian.PutUint64(data[16:], f.Sequence)
	binary.LittleEndian.PutUint64(data[24:], f.SampleIndex)
	for i, sample := range f.PCM {
		binary.LittleEndian.PutUint16(data[HeaderSize+2*i:], uint16(sample))
	}
	return data, nil
}

func ParseFrame(data []byte) (Frame, error) {
	if len(data) != FrameSize || string(data[:4]) != "DJRM" || data[4] != 1 || (data[5] != FromModem && data[5] != ToModem) || binary.LittleEndian.Uint16(data[6:]) != FrameSamples {
		return Frame{}, ErrFrame
	}
	f := Frame{Direction: data[5], Generation: binary.LittleEndian.Uint64(data[8:]), Sequence: binary.LittleEndian.Uint64(data[16:]), SampleIndex: binary.LittleEndian.Uint64(data[24:])}
	if f.Generation == 0 || f.SampleIndex > math.MaxUint64-FrameSamples {
		return Frame{}, ErrFrame
	}
	for i := range f.PCM {
		f.PCM[i] = int16(binary.LittleEndian.Uint16(data[HeaderSize+2*i:]))
	}
	return f, nil
}

// FrameGuard is used by one reader on an ordered local transport. Gaps are
// allowed (a bounded sender may drop old audio), replay and overlap are not.
type FrameGuard struct {
	Generation    uint64
	Direction     byte
	started       bool
	sequence, end uint64
}

func (g *FrameGuard) Accept(f Frame) error {
	if g.Generation == 0 || f.Generation != g.Generation || f.Direction != g.Direction || f.SampleIndex > math.MaxUint64-FrameSamples {
		return ErrFrame
	}
	if g.started && (f.Sequence <= g.sequence || f.SampleIndex < g.end) {
		return ErrStale
	}
	g.started = true
	g.sequence = f.Sequence
	g.end = f.SampleIndex + FrameSamples
	return nil
}
