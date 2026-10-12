package remotemedia

import (
	"math"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"github.com/pion/rtcp"
)

// FeedbackStats contains no identities, addresses or audio. A report describes
// the phone's reception of the Hub's outgoing SSRC, not the reverse direction.
type FeedbackStats struct {
	Reports      uint64  `json:"reports"`
	Bitrate      int     `json:"bitrate"`
	LossPercent  int     `json:"loss_percent"`
	JitterMS     float64 `json:"jitter_ms"`
	RTTMS        float64 `json:"rtt_ms"`
	RTTAvailable bool    `json:"rtt_available"`
	Changes      uint64  `json:"changes"`
}

type encoderConfiguration interface{ Configure(opus.Config) error }
type feedbackController struct {
	mu                                  sync.Mutex
	stats                               FeedbackStats
	config                              opus.Config
	lastSequence                        uint32
	haveSequence                        bool
	lastReport, lastChange, stableSince time.Time
	baseRTT                             float64
	congested                           int
}

func newFeedbackController() *feedbackController {
	c := opus.DefaultConfig()
	return &feedbackController{config: c, stats: FeedbackStats{Bitrate: c.Bitrate, LossPercent: c.PacketLossPercent}}
}
func (c *feedbackController) snapshot() FeedbackStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// compactNTP is the middle 32 bits used by RR LSR/DLSR (RFC 3550).
func compactNTP(now time.Time) uint32 {
	seconds := uint64(now.Unix() + 2208988800)
	return uint32(seconds<<16 | uint64(now.Nanosecond())*65536/1000000000)
}
func (c *feedbackController) observe(report rtcp.ReceptionReport, ownSSRC uint32, now time.Time, encoder encoderConfiguration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ownSSRC == 0 || report.SSRC != ownSSRC {
		return nil
	}
	if c.haveSequence && int32(report.LastSequenceNumber-c.lastSequence) <= 0 {
		return nil
	}
	if !c.lastReport.IsZero() && now.Before(c.lastReport) {
		return nil
	}
	if !c.lastReport.IsZero() && now.Sub(c.lastReport) > 10*time.Second {
		c.stableSince = time.Time{}
		c.congested = 0
	}
	c.lastReport, c.lastSequence, c.haveSequence = now, report.LastSequenceNumber, true
	c.stats.Reports++
	loss := int(math.Ceil(float64(report.FractionLost) * 100 / 256))
	c.stats.LossPercent = loss
	c.stats.JitterMS = float64(report.Jitter) / 48 // Opus RTP clock is always 48 kHz.
	c.stats.RTTAvailable = false
	c.stats.RTTMS = 0
	if report.LastSenderReport != 0 {
		units := compactNTP(now) - report.LastSenderReport - report.Delay
		ms := float64(units) * 1000 / 65536
		if ms > 0 && ms <= 10000 {
			c.stats.RTTMS, c.stats.RTTAvailable = ms, true
			if c.baseRTT == 0 || ms < c.baseRTT {
				c.baseRTT = ms
			}
		}
	}
	// Loss alone need not mean congestion: preserve bitrate for isolated loss
	// and supply the Opus FEC estimator instead. Require persistent loss plus
	// delay/jitter evidence before reducing bitrate.
	congestion := loss >= 5 && (c.stats.JitterMS >= 20 || c.stats.RTTAvailable && c.stats.RTTMS > c.baseRTT+50)
	if congestion {
		c.congested++
		c.stableSince = time.Time{}
	} else {
		c.congested = 0
		healthy := loss <= 1 && c.stats.JitterMS < 20 && (!c.stats.RTTAvailable || c.stats.RTTMS <= c.baseRTT+30)
		if !healthy {
			c.stableSince = time.Time{}
		} else if c.stableSince.IsZero() {
			c.stableSince = now
		}
	}
	if !c.lastChange.IsZero() && now.Sub(c.lastChange) < 5*time.Second {
		return nil
	}
	next := c.config
	next.PacketLossPercent = min(30, loss)
	if c.congested >= 2 {
		next.Bitrate = max(12000, next.Bitrate-2000)
	}
	if !c.stableSince.IsZero() && now.Sub(c.stableSince) >= 20*time.Second {
		next.Bitrate = min(24000, next.Bitrate+2000)
	}
	if next == c.config {
		return nil
	}
	if err := encoder.Configure(next); err != nil {
		return err
	}
	if next.Bitrate > c.config.Bitrate {
		c.stableSince = now
	}
	c.config, c.lastChange = next, now
	c.stats.Bitrate = next.Bitrate
	c.stats.Changes++
	return nil
}
