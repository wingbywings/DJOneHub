//go:build cgo && remoteopus

package opus

import (
	"errors"
	"math"
	"testing"
)

func TestRealOpusVoiceRoundTripAndRecovery(t *testing.T) {
	t.Logf("using %s", Version())
	codec, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer codec.Close()
	var energy int64
	for frame := 0; frame < 20; frame++ {
		pcm := make([]int16, EncodeSamples)
		for i := range pcm {
			pcm[i] = int16(9000 * math.Sin(2*math.Pi*440*float64(frame*EncodeSamples+i)/8000))
		}
		packet, err := codec.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		if len(packet) == 0 || len(packet) >= len(pcm)*2 {
			t.Fatalf("not compressed: %d", len(packet))
		}
		if frame == 5 {
			lost, err := codec.Conceal(160)
			if err != nil || len(lost) != 160 {
				t.Fatalf("PLC=%d %v", len(lost), err)
			}
			continue
		}
		if frame == 9 {
			continue
		}
		if frame == 10 {
			recovered, err := codec.Recover(packet, 160)
			if err != nil || len(recovered) != 160 {
				t.Fatalf("FEC/PLC=%d %v", len(recovered), err)
			}
		}
		decoded, err := codec.Decode(packet)
		if err != nil || len(decoded) != 160 {
			t.Fatalf("decoded=%d err=%v", len(decoded), err)
		}
		for _, sample := range decoded {
			energy += int64(sample) * int64(sample)
		}
	}
	if energy < 1_000_000 {
		t.Fatalf("decoded silence: %d", energy)
	}
	if err := codec.Configure(Config{Bitrate: 12000, PacketLossPercent: 15}); err != nil {
		t.Fatal(err)
	}
	codec.Close()
	codec.Close()
	if _, err := codec.Encode(make([]int16, 160)); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestVariablePacketDurationAndInvalidInput(t *testing.T) {
	codec, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer codec.Close()
	packet, err := codec.Encode(make([]int16, 160))
	if err != nil {
		t.Fatal(err)
	}
	if packet[0]&3 != 0 {
		t.Fatalf("test expected single 20ms frame, TOC=%x", packet[0])
	}
	for _, count := range []int{2, 6} {
		combined := []byte{packet[0]&0xfc | 3, byte(count)}
		for range count {
			combined = append(combined, packet[1:]...)
		}
		pcm, err := codec.Decode(combined)
		if err != nil || len(pcm) != 160*count {
			t.Fatalf("%d frames: samples=%d err=%v", count, len(pcm), err)
		}
	}
	if _, err := codec.Decode(nil); err == nil {
		t.Fatal("empty packet accepted as audio")
	}
	if _, err := codec.Encode(make([]int16, 159)); err == nil {
		t.Fatal("partial encoder frame accepted")
	}
	if _, err := codec.Recover(packet, 159); err == nil {
		t.Fatal("invalid FEC interval accepted")
	}
	if _, err := codec.Conceal(961); err == nil {
		t.Fatal("unbounded PLC accepted")
	}
	if _, err := New(Config{Bitrate: 999999}); err == nil {
		t.Fatal("invalid bitrate accepted")
	}
}
