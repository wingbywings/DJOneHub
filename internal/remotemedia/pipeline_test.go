//go:build cgo && remoteopus

package remotemedia

import (
	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"testing"
	"time"
)

func TestPCMToRealOpusToPCM(t *testing.T) {
	encoder, err := opus.New(opus.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	decoder, err := opus.New(opus.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	sender, _ := NewSender(1, encoder)
	now := time.Now()
	for i := 0; i < 20; i++ {
		for j := 0; j < 2; j++ {
			frame := Frame{Direction: FromModem, Generation: 1, Sequence: uint64(i*2 + j), SampleIndex: uint64(i*160 + j*80)}
			for k := range frame.PCM {
				frame.PCM[k] = int16((i*160+j*80+k)%100*100 - 5000)
			}
			wire, _ := frame.MarshalBinary()
			parsed, err := ParseFrame(wire)
			if err != nil {
				t.Fatal(err)
			}
			if err := sender.Push(parsed, now); err != nil {
				t.Fatal(err)
			}
		}
		packet, err := sender.Next(now)
		if err != nil || packet == nil {
			t.Fatalf("packet=%v err=%v", packet, err)
		}
		if packet.Timestamp != uint32(i*960) {
			t.Fatalf("RTP clock=%d", packet.Timestamp)
		}
		pcm, err := decoder.Decode(packet.Payload)
		if err != nil || len(pcm) != 160 {
			t.Fatalf("decode=%d %v", len(pcm), err)
		}
		now = now.Add(20 * time.Millisecond)
	}
}
