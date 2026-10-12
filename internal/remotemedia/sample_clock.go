package remotemedia

import "time"

// sampleClock slowly corrects independent hardware clocks from queue occupancy.
// Network jitter is low-pass filtered; correction is capped at 1000 ppm and
// slewed at 50 ppm/s. Rendering interpolates individual samples, never drops or
// duplicates whole speech frames. Reset on an actual rebuffer, not on packet loss.
type sampleClock struct {
	started, last                  time.Time
	filtered, integral, ppm, phase float64
}

func (c *sampleClock) update(errorSamples float64, now time.Time) {
	if c.started.IsZero() {
		c.started, c.last = now, now
		return
	}
	dt := now.Sub(c.last).Seconds()
	c.last = now
	if dt <= 0 || dt > .1 {
		return
	}
	c.filtered += min(1, dt/10) * (errorSamples - c.filtered)
	if now.Sub(c.started) < 3*time.Second {
		return
	}
	c.integral = max(-1000, min(1000, c.integral+c.filtered*.1*dt))
	target := max(-1000, min(1000, 2*c.filtered+c.integral))
	c.ppm += max(-50*dt, min(50*dt, target-c.ppm))
}
func (c *sampleClock) needed() int {
	if c.ppm == 0 && c.phase == 0 {
		return FrameSamples
	}
	return int(c.phase+float64(FrameSamples-1)*(1+c.ppm/1e6)) + 2
}
func (c *sampleClock) render(input []int16, output []int16) int {
	if c.ppm == 0 && c.phase == 0 {
		copy(output, input)
		return len(output)
	}
	step := 1 + c.ppm/1e6
	for i := range output {
		p := c.phase + float64(i)*step
		index := int(p)
		fraction := p - float64(index)
		value := float64(input[index]) + float64(int32(input[index+1])-int32(input[index]))*fraction
		output[i] = int16(max(-32768, min(32767, value)))
	}
	end := c.phase + float64(len(output))*step
	consumed := int(end)
	c.phase = end - float64(consumed)
	return consumed
}
