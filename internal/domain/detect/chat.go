package detect

import (
	"strings"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ChatConfig configures the chat activity detector.
type ChatConfig struct {
	// Window of the sliding message rate (e.g. 10 s).
	Window time.Duration
	// Sustain: the reported value is the minimum z over this trailing period,
	// so the z-score must stay high for that long (e.g. 4 s).
	Sustain time.Duration
	// Alpha is the EWMA smoothing factor of the baseline.
	Alpha float64
	// Terms maps laughter/hype emotes and words to message weights (> 1).
	Terms map[string]float64
	// MinViewers floors the viewer normalization.
	MinViewers int
	// MinStd floors the baseline standard deviation (normalized rate units).
	MinStd float64
	// Retention is how much history is kept.
	Retention time.Duration
}

func (c ChatConfig) withDefaults() ChatConfig {
	if c.Window <= 0 {
		c.Window = 10 * time.Second
	}
	if c.Sustain <= 0 {
		c.Sustain = time.Second
	}
	if c.MinViewers <= 0 {
		c.MinViewers = 50
	}
	if c.MinStd <= 0 {
		c.MinStd = 0.05
	}
	if c.Retention <= 0 {
		c.Retention = 15 * time.Minute
	}
	return c
}

// Chat measures the weighted chat rate per second, normalized by viewers and
// by an adaptive baseline, and requires sustained activity.
type Chat struct {
	cfg      ChatConfig
	baseline *EWMA
	viewers  int

	buckets  map[int64]float64 // weighted message count per second
	zs       map[int64]float64 // z-score per second
	next     int64             // next second to process
	started  bool
	oldestZ  int64
	windowSz int64
	sustain  int64
}

var _ Detector = (*Chat)(nil)

// NewChat builds a chat detector.
func NewChat(cfg ChatConfig) *Chat {
	cfg = cfg.withDefaults()
	return &Chat{
		cfg:      cfg,
		baseline: NewEWMA(cfg.Alpha, cfg.MinStd),
		buckets:  make(map[int64]float64),
		zs:       make(map[int64]float64),
		windowSz: max(int64(cfg.Window/time.Second), 1),
		sustain:  max(int64(cfg.Sustain/time.Second), 1),
	}
}

// Kind returns SignalChat.
func (c *Chat) Kind() domain.SignalKind { return domain.SignalChat }

// SetViewers updates the viewer count used for normalization.
func (c *Chat) SetViewers(n int) { c.viewers = n }

// Weight is the weight of one message: the largest weight among the
// configured terms it contains, at least 1.
func (c *Chat) Weight(text string) float64 {
	w := 1.0
	for term, tw := range c.cfg.Terms {
		if tw > w && term != "" && strings.Contains(text, term) {
			w = tw
		}
	}
	return w
}

// Observe records a message at its aligned position.
func (c *Chat) Observe(pos domain.StreamPos, text string) {
	s := second(pos)
	if c.started && s < c.next-c.windowSz {
		return // older than anything that can still affect a window
	}
	c.buckets[s] += c.Weight(text)
}

// Advance computes the z-score of every whole second up to pos.
func (c *Chat) Advance(pos domain.StreamPos) {
	target := second(pos)
	if !c.started {
		c.next, c.oldestZ, c.started = target, target, true
	}
	if target-c.next > int64(c.cfg.Retention/time.Second) {
		// Long gap (e.g. capture restarted): skip ahead instead of replaying it.
		c.next = target - int64(c.cfg.Retention/time.Second)
	}
	viewers := max(c.viewers, c.cfg.MinViewers)
	for ; c.next <= target; c.next++ {
		sum := 0.0
		for s := c.next - c.windowSz + 1; s <= c.next; s++ {
			sum += c.buckets[s]
		}
		rate := sum / float64(c.windowSz)
		normalized := rate * 1000 / float64(viewers)
		c.zs[c.next] = c.baseline.Observe(normalized)
	}
	c.trim(target)
}

func (c *Chat) trim(now int64) {
	keep := int64(c.cfg.Retention / time.Second)
	for ; c.oldestZ < now-keep; c.oldestZ++ {
		delete(c.zs, c.oldestZ)
		delete(c.buckets, c.oldestZ-c.windowSz)
	}
}

// Value is the sustained z-score: the minimum over the trailing Sustain
// seconds ending at pos (0 if any of them was never computed).
func (c *Chat) Value(pos domain.StreamPos) float64 {
	end := second(pos)
	lo := 0.0
	for s := end - c.sustain + 1; s <= end; s++ {
		z, ok := c.zs[s]
		if !ok {
			return 0
		}
		if s == end-c.sustain+1 || z < lo {
			lo = z
		}
	}
	return lo
}

// RawZ is the instantaneous z-score of the second containing pos.
func (c *Chat) RawZ(pos domain.StreamPos) float64 { return c.zs[second(pos)] }
