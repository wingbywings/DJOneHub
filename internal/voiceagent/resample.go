package voiceagent

import (
	"encoding/binary"
	"math"
)

// StreamingPCM16Downsampler preserves FIR history and decimation phase across
// provider audio deltas. It is optimized for integer-rate mono PCM16
// downsampling such as the Realtime API's 24 kHz output to 8 kHz telephony.
type StreamingPCM16Downsampler struct {
	factor       int
	coefficients []float64
	history      []float64
	position     int
	inputCount   uint64
	pendingByte  byte
	hasPending   bool
}

// StreamingPCM16Upsampler linearly interpolates integer-rate mono PCM16 while
// retaining the previous input sample across media messages. The first source
// sample is repeated to establish the clock; later samples interpolate from
// the previous message instead of flattening each WebSocket edge into a click.
type StreamingPCM16Upsampler struct {
	factor      int
	previous    int16
	hasPrevious bool
	pendingByte byte
	hasPending  bool
}

func NewStreamingPCM16Upsampler(fromRate, toRate int) *StreamingPCM16Upsampler {
	if fromRate <= 0 || toRate <= fromRate || toRate%fromRate != 0 {
		return nil
	}
	return &StreamingPCM16Upsampler{factor: toRate / fromRate}
}

func (r *StreamingPCM16Upsampler) Process(input []byte) []byte {
	if r == nil || len(input) == 0 {
		return nil
	}
	if r.hasPending {
		input = append([]byte{r.pendingByte}, input...)
		r.hasPending = false
	}
	if len(input)%2 != 0 {
		r.pendingByte = input[len(input)-1]
		r.hasPending = true
		input = input[:len(input)-1]
	}
	output := make([]byte, 0, len(input)*r.factor)
	for offset := 0; offset < len(input); offset += 2 {
		current := int16(binary.LittleEndian.Uint16(input[offset:]))
		if !r.hasPrevious {
			r.previous = current
			r.hasPrevious = true
			for range r.factor {
				var encoded [2]byte
				binary.LittleEndian.PutUint16(encoded[:], uint16(current))
				output = append(output, encoded[:]...)
			}
			continue
		}
		start := int64(r.previous)
		difference := int64(current) - start
		for phase := 1; phase <= r.factor; phase++ {
			value := start + difference*int64(phase)/int64(r.factor)
			var encoded [2]byte
			binary.LittleEndian.PutUint16(encoded[:], uint16(int16(value)))
			output = append(output, encoded[:]...)
		}
		r.previous = current
	}
	return output
}

func NewStreamingPCM16Downsampler(fromRate, toRate int) *StreamingPCM16Downsampler {
	if fromRate <= toRate || toRate <= 0 || fromRate%toRate != 0 {
		return nil
	}
	const radius = 16
	cutoff := 0.45 * float64(toRate) / float64(fromRate)
	coefficients := make([]float64, radius*2+1)
	var sum float64
	for index := range coefficients {
		distance := float64(index - radius)
		kernel := 2 * cutoff
		if distance != 0 {
			kernel = math.Sin(2*math.Pi*cutoff*distance) / (math.Pi * distance)
		}
		window := 0.42 + 0.5*math.Cos(math.Pi*distance/radius) + 0.08*math.Cos(2*math.Pi*distance/radius)
		coefficients[index] = kernel * window
		sum += coefficients[index]
	}
	for index := range coefficients {
		coefficients[index] /= sum
	}
	return &StreamingPCM16Downsampler{
		factor: fromRate / toRate, coefficients: coefficients, history: make([]float64, len(coefficients)),
	}
}

func (r *StreamingPCM16Downsampler) Process(input []byte) []byte {
	if r == nil || len(input) == 0 {
		return nil
	}
	if r.hasPending {
		input = append([]byte{r.pendingByte}, input...)
		r.hasPending = false
	}
	if len(input)%2 != 0 {
		r.pendingByte = input[len(input)-1]
		r.hasPending = true
		input = input[:len(input)-1]
	}
	output := make([]byte, 0, len(input)/r.factor)
	for offset := 0; offset < len(input); offset += 2 {
		r.history[r.position] = float64(int16(binary.LittleEndian.Uint16(input[offset:])))
		r.position = (r.position + 1) % len(r.history)
		r.inputCount++
		if r.inputCount%uint64(r.factor) != 0 {
			continue
		}
		var value float64
		for index, coefficient := range r.coefficients {
			historyIndex := r.position - 1 - index
			if historyIndex < 0 {
				historyIndex += len(r.history)
			}
			value += r.history[historyIndex] * coefficient
		}
		value = math.Max(-32768, math.Min(32767, value))
		var encoded [2]byte
		binary.LittleEndian.PutUint16(encoded[:], uint16(int16(math.Round(value))))
		output = append(output, encoded[:]...)
	}
	return output
}

// ResamplePCM16 converts mono little-endian signed PCM. Telephone frames are
// short (normally 20-40 ms), so linear interpolation is a good low-latency
// baseline. Provider adapters own this conversion and the hardware bridge
// remains fixed at the modem-native 8 kHz clock.
func ResamplePCM16(input []byte, fromRate, toRate int) []byte {
	if len(input) < 2 || fromRate <= 0 || toRate <= 0 {
		return nil
	}
	input = input[:len(input)-len(input)%2]
	if fromRate == toRate {
		return append([]byte(nil), input...)
	}
	inFrames := len(input) / 2
	samples := make([]float64, inFrames)
	for i := range samples {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(input[i*2:])))
	}
	if fromRate > toRate {
		samples = lowPassForDownsampling(samples, fromRate, toRate)
	}
	outFrames := (inFrames*toRate + fromRate - 1) / fromRate
	output := make([]byte, outFrames*2)
	for i := 0; i < outFrames; i++ {
		position := float64(i*fromRate) / float64(toRate)
		lower := int(position)
		if lower >= inFrames-1 {
			binary.LittleEndian.PutUint16(output[i*2:], uint16(int16(math.Round(samples[inFrames-1]))))
			continue
		}
		fraction := position - float64(lower)
		value := int16(math.Round(samples[lower]*(1-fraction) + samples[lower+1]*fraction))
		binary.LittleEndian.PutUint16(output[i*2:], uint16(value))
	}
	return output
}

// lowPassForDownsampling applies a short Blackman-windowed sinc filter before
// samples are discarded. Without this step, OpenAI's 24 kHz speech aliases
// high-frequency energy back into the modem's 0-4 kHz telephone band.
func lowPassForDownsampling(input []float64, fromRate, toRate int) []float64 {
	const radius = 16
	cutoff := 0.45 * float64(toRate) / float64(fromRate)
	if cutoff <= 0 || cutoff >= 0.5 || len(input) < 2 {
		return input
	}
	output := make([]float64, len(input))
	for index := range input {
		var value, weightSum float64
		for offset := -radius; offset <= radius; offset++ {
			distance := float64(offset)
			kernel := 2 * cutoff
			if offset != 0 {
				kernel = math.Sin(2*math.Pi*cutoff*distance) / (math.Pi * distance)
			}
			window := 0.42 + 0.5*math.Cos(math.Pi*distance/radius) + 0.08*math.Cos(2*math.Pi*distance/radius)
			weight := kernel * window
			source := index + offset
			if source < 0 {
				source = 0
			} else if source >= len(input) {
				source = len(input) - 1
			}
			value += input[source] * weight
			weightSum += weight
		}
		if weightSum != 0 {
			value /= weightSum
		}
		output[index] = math.Max(-32768, math.Min(32767, value))
	}
	return output
}
