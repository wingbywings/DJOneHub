// Package opus provides an optional libopus voice codec (1.5.2 or 1.6.1). Build with
// -tags remoteopus and CGO_ENABLED=1. A default build fails closed.
package opus

import "errors"

var ErrUnavailable = errors.New("Opus support requires CGO and the remoteopus build tag")
var ErrClosed = errors.New("Opus codec is closed")
var ErrInput = errors.New("invalid Opus audio input")

const SampleRate = 8000
const EncodeSamples = 160
const MaxDecodeSamples = 960 // Opus packets can contain up to 120 ms.
const MaxPacketBytes = 1275  // Bound accepted single-stream WebRTC payloads.
type Config struct{ Bitrate, PacketLossPercent int }

func DefaultConfig() Config { return Config{Bitrate: 16000, PacketLossPercent: 5} }
func (c Config) validate() error {
	if c.Bitrate < 12000 || c.Bitrate > 24000 || c.PacketLossPercent < 0 || c.PacketLossPercent > 100 {
		return ErrInput
	}
	return nil
}
func validRecoverySize(samples int) bool {
	return samples > 0 && samples <= MaxDecodeSamples && samples%20 == 0
}
