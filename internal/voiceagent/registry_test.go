package voiceagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
)

type testProvider struct{ name string }

func (p testProvider) Name() string                                       { return p.name }
func (testProvider) Mode() Mode                                           { return ModeRealtime }
func (testProvider) Open(context.Context, SessionConfig) (Session, error) { return nil, nil }

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(testProvider{name: "Qwen"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(testProvider{name: "qwen"}); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	if names := r.Names(); len(names) != 1 || names[0] != "qwen" {
		t.Fatalf("names = %#v", names)
	}
}

func TestResamplePCM16AttenuatesAliasingAboveTelephoneBand(t *testing.T) {
	const inputRate = 24000
	input := make([]byte, inputRate/10*2)
	for i := 0; i < len(input)/2; i++ {
		sample := int16(math.Sin(2*math.Pi*7000*float64(i)/inputRate) * 20000)
		binary.LittleEndian.PutUint16(input[i*2:], uint16(sample))
	}
	output := ResamplePCM16(input, inputRate, 8000)
	var energy float64
	for i := 0; i < len(output)/2; i++ {
		value := float64(int16(binary.LittleEndian.Uint16(output[i*2:])))
		energy += value * value
	}
	rms := math.Sqrt(energy / float64(len(output)/2))
	if rms > 2500 {
		t.Fatalf("aliased 7 kHz RMS = %.1f, want <= 2500", rms)
	}
}

func TestStreamingDownsamplerIsContinuousAcrossProviderDeltas(t *testing.T) {
	const inputRate = 24000
	input := make([]byte, inputRate*3/2*2)
	for i := 0; i < len(input)/2; i++ {
		value := math.Sin(2*math.Pi*440*float64(i)/inputRate)*14000 +
			math.Sin(2*math.Pi*1800*float64(i)/inputRate)*5000
		binary.LittleEndian.PutUint16(input[i*2:], uint16(int16(value)))
	}
	whole := NewStreamingPCM16Downsampler(inputRate, 8000).Process(input)
	chunkedResampler := NewStreamingPCM16Downsampler(inputRate, 8000)
	var chunked []byte
	for offset, sizeIndex := 0, 0; offset < len(input); sizeIndex++ {
		sizes := []int{317, 960, 81, 2048, 511}
		size := sizes[sizeIndex%len(sizes)]
		end := min(len(input), offset+size)
		chunked = append(chunked, chunkedResampler.Process(input[offset:end])...)
		offset = end
	}
	if len(chunked) != len(whole) {
		t.Fatalf("chunked length = %d, whole = %d", len(chunked), len(whole))
	}
	for index := range whole {
		if chunked[index] != whole[index] {
			t.Fatalf("chunk boundary changed byte %d: chunked=%d whole=%d", index, chunked[index], whole[index])
		}
	}
}

func TestStreamingDownsamplerKeepsLongAudioStable(t *testing.T) {
	const (
		inputRate = 24000
		seconds   = 30
	)
	resampler := NewStreamingPCM16Downsampler(inputRate, 8000)
	var output []byte
	for start := 0; start < inputRate*seconds; {
		frames := min(487, inputRate*seconds-start)
		chunk := make([]byte, frames*2)
		for frame := 0; frame < frames; frame++ {
			index := start + frame
			value := math.Sin(2*math.Pi*700*float64(index)/inputRate) * 12000
			binary.LittleEndian.PutUint16(chunk[frame*2:], uint16(int16(value)))
		}
		output = append(output, resampler.Process(chunk)...)
		start += frames
	}
	if got, want := len(output), 8000*seconds*2; got != want {
		t.Fatalf("long output length = %d, want %d", got, want)
	}
	rms := func(fromSecond, toSecond int) float64 {
		start := fromSecond * 8000
		end := toSecond * 8000
		var energy float64
		for frame := start; frame < end; frame++ {
			value := float64(int16(binary.LittleEndian.Uint16(output[frame*2:])))
			energy += value * value
		}
		return math.Sqrt(energy / float64(end-start))
	}
	first, last := rms(1, 5), rms(seconds-5, seconds-1)
	if ratio := last / first; ratio < 0.99 || ratio > 1.01 {
		t.Fatalf("long audio RMS drift: first=%.1f last=%.1f ratio=%.4f", first, last, ratio)
	}
}

func TestStreamingUpsamplerIsContinuousAcrossProviderMessages(t *testing.T) {
	input := make([]byte, 9*2)
	values := []int16{-12000, -8000, -3000, 2000, 9000, 4000, -1000, -7000, -11000}
	for index, value := range values {
		binary.LittleEndian.PutUint16(input[index*2:], uint16(value))
	}
	whole := NewStreamingPCM16Upsampler(8000, 16000).Process(input)
	chunkedResampler := NewStreamingPCM16Upsampler(8000, 16000)
	var chunked []byte
	for _, part := range [][]byte{input[:3], input[3:8], input[8:13], input[13:]} {
		chunked = append(chunked, chunkedResampler.Process(part)...)
	}
	if !bytes.Equal(chunked, whole) {
		t.Fatalf("chunked upsampling differs across odd-byte boundaries\nchunked=%v\nwhole=%v", chunked, whole)
	}
	if got, want := len(whole), len(values)*2*2; got != want {
		t.Fatalf("upsampled length = %d, want %d", got, want)
	}
	for source := 1; source < len(values); source++ {
		frame := source * 2
		left := values[source-1]
		middle := int16(binary.LittleEndian.Uint16(whole[frame*2:]))
		right := values[source]
		want := int16((int32(left) + int32(right)) / 2)
		if middle != want {
			t.Fatalf("interpolated frame %d = %d, want %d", frame, middle, want)
		}
	}
}

func TestResamplePCM16(t *testing.T) {
	input := make([]byte, 160*2)
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint16(input[i*2:], uint16(int16(i*100)))
	}
	up := ResamplePCM16(input, 8000, 24000)
	if len(up) != len(input)*3 {
		t.Fatalf("up length = %d", len(up))
	}
	down := ResamplePCM16(up, 24000, 8000)
	if len(down) != len(input) {
		t.Fatalf("down length = %d", len(down))
	}
}
