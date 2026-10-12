package remotemedia

import (
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"
)

func TestReceiverRecoversFromSustainedNetworkDelay(t *testing.T) {
	r, _ := receiverForTest()
	start := time.Unix(1, 0)
	// Shift one-way delay by 100ms after 1s. Unlike a scheduler stall, reads
	// keep running throughout; old fixed buffering rejected the rest forever.
	next := 0
	for tick := 0; tick < 2000; tick++ {
		now := start.Add(time.Duration(tick) * 10 * time.Millisecond)
		for next < 990 {
			arrival := next * 2
			if next >= 50 {
				arrival += 10
			}
			if arrival > tick {
				break
			}
			err := r.Push(rtpInput(uint16(next), uint32(next*960), byte(next%100+1)), now)
			if err != nil && !errors.Is(err, ErrRTPWindow) {
				t.Fatal(err)
			}
			next++
		}
		if _, err := r.Read(now); err != nil {
			t.Fatal(err)
		}
	}
	stats := r.Stats()
	if stats.DecodedPackets < 970 || stats.LateRecoveries == 0 || stats.RejectedPackets != 0 || stats.BufferedPackets > 12 {
		t.Fatalf("delay change did not converge: %+v", stats)
	}
	if stats.TargetDelayMS < 40 || stats.TargetDelayMS > 120 {
		t.Fatal(stats)
	}
	// Rebuffer may revisit concealed time, never already decoded speech.
	if err := r.Push(rtpInput(1100, 960, 3), start.Add(21*time.Second)); err == nil {
		t.Fatal("decoded speech replayed")
	}
}
func TestReceiverAdaptiveDelayBoundedAndUntrustedInputCannotReset(t *testing.T) {
	r, _ := receiverForTest()
	start := time.Unix(1, 0)
	// Deliver jittered packets in real arrival order while the output clock
	// continues at 10 ms, independently of RTP delivery and reordering.
	type delivery struct{ seq, tick int }
	arrivals := make([]delivery, 1000)
	for i := range arrivals {
		arrivals[i] = delivery{i, i * 2}
		if i%2 == 1 {
			arrivals[i].tick += 6
		}
	}
	sort.SliceStable(arrivals, func(i, j int) bool { return arrivals[i].tick < arrivals[j].tick })
	next := 0
	for tick := 0; tick < 2010; tick++ {
		now := start.Add(time.Duration(tick) * 10 * time.Millisecond)
		for next < len(arrivals) && arrivals[next].tick <= tick {
			i := arrivals[next].seq
			err := r.Push(rtpInput(uint16(i), uint32(i*960), 1), now)
			if err != nil && !errors.Is(err, ErrRTPWindow) {
				t.Fatal(err)
			}
			next++
		}
		if _, err := r.Read(now); err != nil {
			t.Fatal(err)
		}
	}
	before := r.Stats()
	packet := rtpInput(1001, 960960, 1)
	packet.SSRC = 999
	if !errors.Is(r.Push(packet, start.Add(21*time.Second)), ErrRTPStream) {
		t.Fatal("SSRC changed")
	}
	after := r.Stats()
	if after.TargetDelayMS < 40 || after.TargetDelayMS > 120 || after.LateRecoveries != before.LateRecoveries {
		t.Fatal(after)
	}
}

func TestReceiverIndependentSampleClocks(t *testing.T) {
	for _, ppm := range []int64{-500, 500} {
		t.Run(fmt.Sprintf("%dppm", ppm), func(t *testing.T) {
			r, _ := receiverForTest()
			start := time.Unix(1, 0)
			seq := int64(0)
			for tick := int64(0); tick < 3600*100; tick++ { // one simulated hour through the actual jitter buffer
				now := start.Add(time.Duration(tick) * 10 * time.Millisecond)
				for time.Duration(seq)*20*time.Millisecond*1_000_000/time.Duration(1_000_000+ppm) <= now.Sub(start) {
					if err := r.Push(rtpInput(uint16(seq), uint32(seq*960), 1), now); err != nil {
						t.Fatalf("clock %d tick %d: %v %+v", ppm, tick, err, r.Stats())
					}
					seq++
				}
				if _, err := r.Read(now); err != nil {
					t.Fatal(err)
				}
			}
			stats := r.Stats()
			if stats.PLCSamples != 0 || stats.LatePackets != 0 || stats.Rebuffers != 0 || stats.BufferedPackets > 12 {
				t.Fatalf("normal clock mismatch damaged speech: %+v", stats)
			}
		})
	}
}

func TestReceiverRestartRecoversFifteenSecondGapWithoutReplayingAudio(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		r, _ := receiverForTest()
		start := time.Unix(1, 0)
		if err := r.Push(rtpInput(1, 960, 1), start); err != nil {
			t.Fatal(err)
		}
		for tick := 0; tick < 1500; tick++ {
			if _, err := r.Read(start.Add(time.Duration(tick) * 10 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		if enabled {
			r.beginTransportRecovery(start.Add(5 * time.Second))
		}
		resumed := start.Add(15 * time.Second)
		packet := rtpInput(751, 751*960, 2)
		err := r.Push(packet, resumed)
		if !enabled {
			if !errors.Is(err, ErrRTPWindow) {
				t.Fatal("unnegotiated gap accepted", err)
			}
			continue
		}
		if err != nil {
			t.Fatal("restart gap rejected", err)
		}
		before := r.Stats().DecodedPackets
		for tick := 0; tick <= 12; tick++ {
			if _, err := r.Read(resumed.Add(time.Duration(tick) * 10 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		if r.Stats().DecodedPackets != before+1 {
			t.Fatal("resumed packet did not decode")
		}
		if err := r.Push(packet, resumed); !errors.Is(err, ErrRTPDuplicate) {
			t.Fatal("packet replay", err)
		}
		if err := r.Push(rtpInput(752, 960, 3), resumed); err == nil {
			t.Fatal("decoded audio replay")
		}
		if err := r.Push(rtpInput(1501, 1501*960, 3), resumed.Add(15*time.Second)); err == nil {
			t.Fatal("reused gap allowance")
		}
	}
}

func TestReceiverRestartGapRejectsExpiredAndMismatchedInput(t *testing.T) {
	for _, kind := range []string{"expired", "ssrc", "generation", "future", "sequence"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := receiverForTest()
			start := time.Unix(1, 0)
			_ = r.Push(rtpInput(1, 960, 1), start)
			r.beginTransportRecovery(start)
			packet, now := rtpInput(751, 751*960, 2), start.Add(15*time.Second)
			switch kind {
			case "expired":
				now = start.Add(21 * time.Second)
			case "ssrc":
				packet.SSRC++
			case "generation":
				packet.Generation++
			case "future":
				packet.Timestamp += 48000
			case "sequence":
				packet.Sequence = 10000
			}
			if err := r.Push(packet, now); err == nil {
				t.Fatal("invalid gap accepted")
			}
		})
	}
}
