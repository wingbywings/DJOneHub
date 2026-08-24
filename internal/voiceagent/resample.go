package voiceagent

import (
	"encoding/binary"
	"math"
)

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
