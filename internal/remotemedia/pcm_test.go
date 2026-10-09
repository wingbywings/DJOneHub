package remotemedia

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPCMSharedSwiftFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/pcm-v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	data, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ParseFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Generation != 7 || frame.Sequence != 9 || frame.SampleIndex != 720 || frame.PCM[0] != -40 || frame.PCM[79] != 39 {
		t.Fatalf("frame=%+v", frame)
	}
	encoded, _ := frame.MarshalBinary()
	if string(encoded) != string(data) {
		t.Fatal("wire fixture changed")
	}
	guard := FrameGuard{Generation: 7, Direction: FromModem}
	if err := guard.Accept(frame); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(guard.Accept(frame), ErrStale) {
		t.Fatal("replay accepted")
	}
	frame.Sequence++
	frame.SampleIndex += 80
	if err := guard.Accept(frame); err != nil {
		t.Fatal(err)
	}
	frame.Generation++
	if !errors.Is(guard.Accept(frame), ErrFrame) {
		t.Fatal("old generation accepted")
	}
}
func FuzzParseFrame(f *testing.F) {
	data, _ := (Frame{Direction: FromModem, Generation: 1}).MarshalBinary()
	f.Add(data)
	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ParseFrame(data)
		if err != nil {
			return
		}
		encoded, err := frame.MarshalBinary()
		if err != nil || string(encoded) != string(data) {
			t.Fatal("noncanonical frame")
		}
	})
}

type countingEncoder struct{ calls int }

func (e *countingEncoder) Encode(pcm []int16) ([]byte, error) {
	e.calls++
	return []byte{byte(len(pcm))}, nil
}
func TestSenderBoundedClockAndGaps(t *testing.T) {
	enc := &countingEncoder{}
	sender, _ := NewSender(7, enc)
	now := time.Unix(1, 0)
	for i := 0; i < 20; i++ {
		if err := sender.Push(Frame{Direction: FromModem, Generation: 7, Sequence: uint64(i), SampleIndex: uint64(i * 80)}, now); err != nil {
			t.Fatal(err)
		}
	}
	if sender.Stats().QueuedFrames != 6 || sender.Stats().DroppedFrames != 14 {
		t.Fatal(sender.Stats())
	}
	first, err := sender.Next(now)
	if err != nil || first.SampleIndex != 14*80 || first.Timestamp != 14*80*6 {
		t.Fatalf("packet=%+v err=%v", first, err)
	}
	if packet, _ := sender.Next(now.Add(time.Millisecond)); packet != nil {
		t.Fatal("burst catch-up")
	}
	if packet, _ := sender.Next(now.Add(200 * time.Millisecond)); packet != nil {
		t.Fatal("expired capture encoded")
	}
	if enc.calls != 1 {
		t.Fatal(enc.calls)
	}
	for i := 0; i < 2; i++ {
		_ = sender.Push(Frame{Direction: FromModem, Generation: 7, Sequence: uint64(30 + i), SampleIndex: uint64(30+i) * 80}, now.Add(time.Second))
	}
	next, _ := sender.Next(now.Add(time.Second))
	if next.SampleIndex != 30*80 || next.Timestamp <= first.Timestamp {
		t.Fatal("clock restarted after dropped audio")
	}
}
func TestSenderDoesNotJoinAcrossMissingAudio(t *testing.T) {
	sender, _ := NewSender(1, &countingEncoder{})
	now := time.Now()
	for _, index := range []uint64{0, 160, 240} {
		_ = sender.Push(Frame{Direction: FromModem, Generation: 1, Sequence: index, SampleIndex: index}, now)
	}
	packet, err := sender.Next(now)
	if err != nil || packet.SampleIndex != 160 {
		t.Fatalf("packet=%+v err=%v", packet, err)
	}
}
