package remotemedia

import (
	"errors"
	"testing"
	"time"
)

type testDecoder struct{ fec, plc, resets int }

func samplesOf(data []byte) (int, error) {
	if len(data) != 2 {
		return 0, ErrFrame
	}
	return int(data[0]) * 20, nil
}
func repeatPCM(n int, value int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = value
	}
	return out
}
func (d *testDecoder) Decode(data []byte) ([]int16, error) {
	n, err := samplesOf(data)
	return repeatPCM(n, int16(data[1])), err
}
func (d *testDecoder) Recover(data []byte, n int) ([]int16, error) {
	d.fec++
	return repeatPCM(n, int16(data[1])-1), nil
}
func (d *testDecoder) Conceal(n int) ([]int16, error) { d.plc += n; return repeatPCM(n, -9), nil }
func (d *testDecoder) ResetDecoder() error            { d.resets++; return nil }
func rtpInput(seq uint16, ts uint32, value byte) IncomingRTP {
	return IncomingRTP{Generation: 7, SSRC: 123, Sequence: seq, Timestamp: ts, Payload: []byte{8, value}}
}
func receiverForTest() (*Receiver, *testDecoder) {
	d := &testDecoder{}
	r, _ := NewReceiver(7, d, samplesOf)
	return r, d
}
func readValue(t *testing.T, r *Receiver, now time.Time, want int16) {
	t.Helper()
	f, err := r.Read(now)
	if err != nil || f == nil {
		t.Fatalf("frame=%v error=%v", f, err)
	}
	for _, sample := range f.PCM {
		if sample != want {
			t.Fatalf("sample=%d want=%d", sample, want)
		}
	}
}
func TestReceiverReordersBeforeDeadline(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	for _, p := range []IncomingRTP{rtpInput(12, 1920, 12), rtpInput(10, 0, 10), rtpInput(11, 960, 11)} {
		if err := r.Push(p, now); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ := r.Read(now.Add(59 * time.Millisecond)); f != nil {
		t.Fatal("prebuffer bypassed")
	}
	for i := 0; i < 6; i++ {
		readValue(t, r, now.Add(time.Duration(60+10*i)*time.Millisecond), int16(10+i/2))
	}
	if r.Stats().DecodedPackets != 3 {
		t.Fatal(r.Stats())
	}
}
func TestReceiverFECBeforeDecodingNextPacket(t *testing.T) {
	r, d := receiverForTest()
	now := time.Unix(1, 0)
	_ = r.Push(rtpInput(1, 0, 1), now)
	_ = r.Push(rtpInput(3, 1920, 3), now)
	for i := 0; i < 6; i++ {
		readValue(t, r, now.Add(time.Duration(60+10*i)*time.Millisecond), int16(1+i/2))
	}
	if d.fec != 1 || d.plc != 0 {
		t.Fatalf("fec=%d plc=%d", d.fec, d.plc)
	}
}
func TestReceiverLimitsConcealmentAndRejectsStaleStreams(t *testing.T) {
	r, d := receiverForTest()
	now := time.Unix(1, 0)
	packet := rtpInput(1, 0, 1)
	if err := r.Push(packet, now); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(r.Push(packet, now), ErrRTPDuplicate) {
		t.Fatal("duplicate accepted")
	}
	wrong := rtpInput(2, 960, 2)
	wrong.SSRC++
	if !errors.Is(r.Push(wrong, now), ErrRTPStream) {
		t.Fatal("SSRC accepted")
	}
	wrong.SSRC = 123
	wrong.Generation++
	if !errors.Is(r.Push(wrong, now), ErrRTPStream) {
		t.Fatal("generation accepted")
	}
	for i := 0; i < 2; i++ {
		readValue(t, r, now.Add(time.Duration(60+i*10)*time.Millisecond), 1)
	}
	for i := 0; i < 6; i++ {
		readValue(t, r, now.Add(time.Duration(80+i*10)*time.Millisecond), -9)
	}
	readValue(t, r, now.Add(140*time.Millisecond), 0)
	if d.plc != 480 || d.resets != 1 {
		t.Fatalf("unbounded concealment: %+v", d)
	}
	if !errors.Is(r.Push(rtpInput(2, 960, 2), now), ErrRTPWindow) {
		t.Fatal("late audio accepted")
	}
}
func TestReceiverHandlesSequenceAndTimestampWrap(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	_ = r.Push(rtpInput(65535, 0xfffffc40, 1), now) // 2^32 - 960
	if err := r.Push(rtpInput(0, 0, 2), now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		readValue(t, r, now.Add(time.Duration(60+i*10)*time.Millisecond), int16(1+i/2))
	}
}
func TestReceiverVariableDurationAndOverlap(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	packet := rtpInput(1, 0, 4)
	packet.Payload[0] = 48 // 120 ms
	if err := r.Push(packet, now); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(r.Push(rtpInput(2, 960, 5), now), ErrRTPDuplicate) {
		t.Fatal("overlap accepted")
	}
	for i := 0; i < 12; i++ {
		readValue(t, r, now.Add(time.Duration(60+i*10)*time.Millisecond), 4)
	}
}
func TestReceiverStallDropsOldDecodedAndQueuedAudio(t *testing.T) {
	r, d := receiverForTest()
	now := time.Unix(1, 0)
	_ = r.Push(rtpInput(1, 0, 1), now)
	_ = r.Push(rtpInput(2, 960, 2), now)
	readValue(t, r, now.Add(60*time.Millisecond), 1)
	if frame, err := r.Read(now.Add(time.Second)); err != nil || frame != nil {
		t.Fatalf("stall=%v %v", frame, err)
	}
	if r.Stats().BufferedPackets != 0 || d.resets != 1 {
		t.Fatal("old packets survived stall")
	}
	if err := r.Push(rtpInput(50, 48000, 50), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	readValue(t, r, now.Add(1060*time.Millisecond), 50)
}
func TestReceiverWindowBoundsAndResetReplay(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	_ = r.Push(rtpInput(1, 0, 1), now)
	if err := r.Push(rtpInput(2, 4800000, 2), now); !errors.Is(err, ErrRTPWindow) {
		t.Fatal("far future accepted")
	}
	if err := r.Push(rtpInput(5000, 960, 2), now); !errors.Is(err, ErrRTPWindow) {
		t.Fatal("sequence jump accepted")
	}
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(rtpInput(1, 0, 1), now); !errors.Is(err, ErrRTPDuplicate) {
		t.Fatal("replay after reset accepted")
	}
	if err := r.Push(rtpInput(2, 960, 2), now); err != nil {
		t.Fatal(err)
	}
}

func TestReceiverRebuffersShortSchedulerPause(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	_ = r.Push(rtpInput(1, 0, 1), now)
	readValue(t, r, now.Add(60*time.Millisecond), 1)
	// New media arrived while the render loop was delayed by 90 ms.
	_ = r.Push(rtpInput(6, 4800, 6), now.Add(150*time.Millisecond))
	if f, err := r.Read(now.Add(160 * time.Millisecond)); err != nil || f != nil {
		t.Fatalf("rebuffer=%v %v", f, err)
	}
	readValue(t, r, now.Add(220*time.Millisecond), 6)
	if r.Stats().Rebuffers != 1 {
		t.Fatal(r.Stats())
	}
}
func TestReceiverCombinesShortLegalOpusDurations(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	for i := 0; i < 4; i++ {
		packet := rtpInput(uint16(i), uint32(i*120), byte(i+1))
		packet.Payload[0] = 1
		if err := r.Push(packet, now); err != nil {
			t.Fatal(err)
		}
	}
	frame, err := r.Read(now.Add(60 * time.Millisecond))
	if err != nil || frame == nil {
		t.Fatalf("frame=%v error=%v", frame, err)
	}
	for i, sample := range frame.PCM {
		if sample != int16(i/20+1) {
			t.Fatalf("sample[%d]=%d", i, sample)
		}
	}
}

func TestReceiverMaintainsRateWithTickerJitter(t *testing.T) {
	r, _ := receiverForTest()
	start := time.Unix(1, 0)
	for i := 0; i < 500; i++ {
		now := start.Add(time.Duration(i)*10*time.Millisecond + time.Duration(500-i)*time.Microsecond)
		if i%2 == 0 {
			if err := r.Push(rtpInput(uint16(i/2), uint32(i*480), 1), now); err != nil {
				t.Fatalf("normal 20 ms input rejected at tick %d: %v; %+v", i, err, r.Stats())
			}
		}
		if _, err := r.Read(now); err != nil {
			t.Fatal(err)
		}
	}
	if stats := r.Stats(); stats.DecodedPackets < 245 || stats.RejectedPackets != 0 || stats.Rebuffers != 0 {
		t.Fatalf("sub-millisecond jitter slowed playout: %+v", stats)
	}
}

func TestMutedReceiverAdvancesWindowWithoutRetainingSpeech(t *testing.T) {
	r, _ := receiverForTest()
	now := time.Unix(1, 0)
	// More than one sequence wrap, representing over twenty minutes of mute.
	for n := 0; n < 70000; n++ {
		packet := rtpInput(uint16(n), uint32(n*960), 9)
		if err := r.Discard(packet, now.Add(time.Duration(n)*20*time.Millisecond)); err != nil {
			t.Fatal(n, err)
		}
	}
	if r.Stats().BufferedPackets != 0 || r.Stats().DecodedPackets != 0 {
		t.Fatal("muted speech retained", r.Stats())
	}
	if frame, _ := r.Read(now.Add(1400 * time.Second)); frame != nil {
		t.Fatal("muted audio played")
	}
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	lastSeq := uint16(69999 % 65536)
	if err := r.Push(rtpInput(lastSeq, 69999*960, 9), now.Add(1400*time.Second)); err == nil {
		t.Fatal("muted packet replayed after unmute")
	}
	freshSeq := uint16(70000 % 65536)
	fresh := rtpInput(freshSeq, 70000*960, 3)
	wrong := fresh
	wrong.SSRC++
	if !errors.Is(r.Discard(wrong, now), ErrRTPStream) {
		t.Fatal("muted SSRC replaced")
	}
	wrong = fresh
	wrong.Generation++
	if !errors.Is(r.Discard(wrong, now), ErrRTPStream) {
		t.Fatal("muted generation replaced")
	}
	resume := now.Add(1400 * time.Second)
	if err := r.Push(fresh, resume); err != nil {
		t.Fatal("unmute locked out", err)
	}
	readValue(t, r, resume.Add(60*time.Millisecond), 3)
}
