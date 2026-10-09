//go:build cgo && remoteopus

package opus

/*
#cgo pkg-config: opus
#include <opus.h>
static int reset_decoder(OpusDecoder *d) { return opus_decoder_ctl(d, OPUS_RESET_STATE); }
static int configure_voice(OpusEncoder *e, int bitrate, int loss) {
 int r;
 #define SET(x) if ((r = opus_encoder_ctl(e, x)) != OPUS_OK) return r
 SET(OPUS_SET_BITRATE(bitrate));
 SET(OPUS_SET_VBR(1));
 SET(OPUS_SET_VBR_CONSTRAINT(1));
 SET(OPUS_SET_COMPLEXITY(5));
 SET(OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE));
 SET(OPUS_SET_MAX_BANDWIDTH(OPUS_BANDWIDTH_NARROWBAND));
 SET(OPUS_SET_INBAND_FEC(1));
 SET(OPUS_SET_PACKET_LOSS_PERC(loss));
 SET(OPUS_SET_DTX(0));
 #undef SET
 return OPUS_OK;
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

type Codec struct {
	mu      sync.Mutex
	encoder *C.OpusEncoder
	decoder *C.OpusDecoder
}

func Available() bool { return true }
func Version() string { return C.GoString(C.opus_get_version_string()) }
func New(config Config) (*Codec, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	var code C.int
	enc := C.opus_encoder_create(SampleRate, 1, C.OPUS_APPLICATION_VOIP, &code)
	if enc == nil || code != C.OPUS_OK {
		return nil, opusError(code)
	}
	if code = C.configure_voice(enc, C.int(config.Bitrate), C.int(config.PacketLossPercent)); code != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, opusError(code)
	}
	dec := C.opus_decoder_create(SampleRate, 1, &code)
	if dec == nil || code != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, opusError(code)
	}
	return &Codec{encoder: enc, decoder: dec}, nil
}
func (c *Codec) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.encoder != nil {
		C.opus_encoder_destroy(c.encoder)
		c.encoder = nil
	}
	if c.decoder != nil {
		C.opus_decoder_destroy(c.decoder)
		c.decoder = nil
	}
}
func (c *Codec) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != EncodeSamples {
		return nil, ErrInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.encoder == nil {
		return nil, ErrClosed
	}
	out := make([]byte, MaxPacketBytes)
	n := C.opus_encode(c.encoder, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), EncodeSamples, (*C.uchar)(unsafe.Pointer(&out[0])), C.opus_int32(len(out)))
	if n < 0 {
		return nil, opusError(C.int(n))
	}
	return out[:int(n)], nil
}
func (c *Codec) Configure(config Config) error {
	if err := config.validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.encoder == nil {
		return ErrClosed
	}
	code := C.configure_voice(c.encoder, C.int(config.Bitrate), C.int(config.PacketLossPercent))
	if code != C.OPUS_OK {
		return opusError(code)
	}
	return nil
}
func PacketSamples(packet []byte) (int, error) {
	if len(packet) == 0 || len(packet) > MaxPacketBytes {
		return 0, ErrInput
	}
	n := C.opus_packet_get_nb_samples((*C.uchar)(unsafe.Pointer(&packet[0])), C.opus_int32(len(packet)), SampleRate)
	if n <= 0 || n > MaxDecodeSamples {
		return 0, ErrInput
	}
	return int(n), nil
}
func (c *Codec) Decode(packet []byte) ([]int16, error) {
	samples, err := PacketSamples(packet)
	if err != nil {
		return nil, err
	}
	return c.decode(packet, samples, false)
}

// Recover asks libopus to recover the preceding interval using the next packet.
// If the packet has no usable FEC, libopus performs PLC; this is not proof of FEC.
// The same packet must subsequently be decoded normally, in order.
func (c *Codec) Recover(nextPacket []byte, missingSamples int) ([]int16, error) {
	if _, err := PacketSamples(nextPacket); err != nil {
		return nil, err
	}
	if !validRecoverySize(missingSamples) {
		return nil, ErrInput
	}
	return c.decode(nextPacket, missingSamples, true)
}
func (c *Codec) Conceal(samples int) ([]int16, error) {
	if !validRecoverySize(samples) {
		return nil, ErrInput
	}
	return c.decode(nil, samples, false)
}
func (c *Codec) decode(packet []byte, samples int, fec bool) ([]int16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decoder == nil {
		return nil, ErrClosed
	}
	var input *C.uchar
	if len(packet) > 0 {
		input = (*C.uchar)(unsafe.Pointer(&packet[0]))
	}
	out := make([]int16, samples)
	flag := C.int(0)
	if fec {
		flag = 1
	}
	n := C.opus_decode(c.decoder, input, C.opus_int32(len(packet)), (*C.opus_int16)(unsafe.Pointer(&out[0])), C.int(samples), flag)
	if n < 0 {
		return nil, opusError(C.int(n))
	}
	return out[:int(n)], nil
}
func opusError(code C.int) error { return fmt.Errorf("libopus: %s", C.GoString(C.opus_strerror(code))) }

func (c *Codec) ResetDecoder() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decoder == nil {
		return ErrClosed
	}
	code := C.reset_decoder(c.decoder)
	if code != C.OPUS_OK {
		return opusError(code)
	}
	return nil
}
