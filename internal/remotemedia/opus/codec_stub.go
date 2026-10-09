//go:build !cgo || !remoteopus

package opus

type Codec struct{}

func Available() bool                               { return false }
func Version() string                               { return "unavailable" }
func New(Config) (*Codec, error)                    { return nil, ErrUnavailable }
func (*Codec) Close()                               {}
func (*Codec) Encode([]int16) ([]byte, error)       { return nil, ErrUnavailable }
func (*Codec) Decode([]byte) ([]int16, error)       { return nil, ErrUnavailable }
func (*Codec) Recover([]byte, int) ([]int16, error) { return nil, ErrUnavailable }
func (*Codec) Conceal(int) ([]int16, error)         { return nil, ErrUnavailable }
func (*Codec) Configure(Config) error               { return ErrUnavailable }
func PacketSamples([]byte) (int, error)             { return 0, ErrUnavailable }

func (*Codec) ResetDecoder() error { return ErrUnavailable }
