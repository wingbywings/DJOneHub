package voiceagent

import (
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
