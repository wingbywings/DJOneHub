package remotemedia

import (
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"github.com/pion/rtcp"
)

type recordingConfiguration struct {
	configs []opus.Config
	err     error
}

func (c *recordingConfiguration) Configure(value opus.Config) error {
	if c.err != nil {
		return c.err
	}
	c.configs = append(c.configs, value)
	return nil
}
func TestFeedbackIsolatesStreamAndIgnoresDuplicateReports(t *testing.T) {
	c := newFeedbackController()
	enc := &recordingConfiguration{}
	now := time.Unix(100, 0)
	rr := rtcp.ReceptionReport{SSRC: 1, LastSequenceNumber: 100, FractionLost: 26, Jitter: 1440}
	if err := c.observe(rr, 2, now, enc); err != nil {
		t.Fatal(err)
	}
	if c.snapshot().Reports != 0 {
		t.Fatal("unrelated SSRC accepted")
	}
	for i := 0; i < 5; i++ {
		if err := c.observe(rr, 1, now.Add(time.Duration(i)*time.Second), enc); err != nil {
			t.Fatal(err)
		}
	}
	if c.snapshot().Reports != 1 || c.snapshot().Bitrate != 16000 {
		t.Fatal(c.snapshot())
	}
}
func TestFeedbackRespondsSlowlyToCongestionAndRecovers(t *testing.T) {
	c := newFeedbackController()
	enc := &recordingConfiguration{}
	now := time.Unix(100, 0)
	send := func(second int, loss uint8, jitter uint32) {
		t.Helper()
		rr := rtcp.ReceptionReport{SSRC: 1, LastSequenceNumber: uint32(100 + second), FractionLost: loss, Jitter: jitter}
		if err := c.observe(rr, 1, now.Add(time.Duration(second)*time.Second), enc); err != nil {
			t.Fatal(err)
		}
	}
	send(0, 26, 0) // isolated loss updates FEC but does not reduce bitrate
	if c.snapshot().Bitrate != 16000 || enc.configs[0].PacketLossPercent != 11 {
		t.Fatal(c.snapshot(), enc.configs)
	}
	send(1, 26, 1440)
	send(2, 26, 1440)
	if c.snapshot().Bitrate != 16000 {
		t.Fatal("changed within 5s")
	}
	send(5, 26, 1440)
	if c.snapshot().Bitrate != 14000 {
		t.Fatal(c.snapshot())
	}
	send(10, 26, 1440)
	send(15, 26, 1440)
	if c.snapshot().Bitrate != 12000 {
		t.Fatal(c.snapshot())
	}
	for i := 16; i <= 40; i++ {
		send(i, 0, 0)
	}
	if c.snapshot().Bitrate != 14000 {
		t.Fatal("healthy 20s window did not recover", c.snapshot())
	}
	for _, cfg := range enc.configs {
		if cfg.Bitrate < 12000 || cfg.Bitrate > 24000 {
			t.Fatal(cfg)
		}
	}
}
func TestFeedbackRTTWrapAndConfigurationFailure(t *testing.T) {
	c := newFeedbackController()
	enc := &recordingConfiguration{err: errors.New("codec closed")}
	now := time.Unix(65536, 0)
	rr := rtcp.ReceptionReport{SSRC: 1, LastSequenceNumber: 1, LastSenderReport: compactNTP(now.Add(-time.Second)), Delay: 32768}
	if err := c.observe(rr, 1, now, enc); err == nil {
		t.Fatal("configuration error swallowed")
	}
	if c.config != opus.DefaultConfig() || c.snapshot().Changes != 0 {
		t.Fatal("failed configuration committed")
	}
	if !c.snapshot().RTTAvailable || c.snapshot().RTTMS != 500 {
		t.Fatal(c.snapshot())
	}
	rr.LastSequenceNumber++
	rr.LastSenderReport = compactNTP(now.Add(time.Second))
	enc.err = nil
	if err := c.observe(rr, 1, now, enc); err != nil {
		t.Fatal(err)
	}
	if c.snapshot().RTTAvailable {
		t.Fatal("impossible RTT accepted")
	}
}
