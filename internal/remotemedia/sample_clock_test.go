package remotemedia

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestSampleClockTracksIndependentClockForThreeSimulatedHours(t *testing.T) {
	for _, drift := range []float64{-500, 500} {
		t.Run(fmt.Sprintf("%gppm", drift), func(t *testing.T) {
			var clock sampleClock
			start := time.Unix(1, 0)
			occupancy := 600.0
			input := make([]int16, 256)
			for i := range input {
				input[i] = 1000
			}
			var output [FrameSamples]int16
			for tick := 0; tick < 3*3600*100; tick++ {
				clock.update(occupancy-600, start.Add(time.Duration(tick)*10*time.Millisecond))
				if clock.needed() > len(input) {
					t.Fatal("unbounded interpolation input")
				}
				consumed := clock.render(input, output[:])
				if consumed < 79 || consumed > 81 {
					t.Fatal("whole audio block correction", consumed)
				}
				occupancy += 80*(1+drift/1e6) - float64(consumed)
				if occupancy < 200 || occupancy > 1000 {
					t.Fatal("clock drift accumulated", tick, occupancy, clock.ppm)
				}
				if output[0] != 1000 || output[79] != 1000 {
					t.Fatal("resampling changed DC gain")
				}
			}
			if math.Abs(clock.ppm-drift) > 10 || math.Abs(occupancy-600) > 5 {
				t.Fatal("clock did not converge", occupancy, clock.ppm)
			}
		})
	}
}
