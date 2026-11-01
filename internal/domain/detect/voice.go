package detect

import (
	"math"
	"sort"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// VoiceConfig configures the voice energy detector.
type VoiceConfig struct {
	// Window aggregates frames (e.g. 400 ms).
	Window time.Duration
	// Alpha is the EWMA smoothing factor of the baseline.
	Alpha float64
	// StaleAfter: with no speech window this recent, the value is 0.
	StaleAfter time.Duration
	// MinSpeechRatio is the fraction of speech frames a window needs.
	MinSpeechRatio float64
	// MinStdDB floors the baseline standard deviation, in dB.
	MinStdDB float64
	// Retention is how much history is kept for Value lookups.
	Retention time.Duration
}

func (c VoiceConfig) withDefaults() VoiceConfig {
	if c.Window <= 0 {
		c.Window = 400 * time.Millisecond
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 2 * time.Second
	}
	if c.MinSpeechRatio <= 0 || c.MinSpeechRatio > 1 {
		c.MinSpeechRatio = 0.5
	}
	if c.MinStdDB <= 0 {
		c.MinStdDB = 1.5
	}
	if c.Retention <= 0 {
		c.Retention = 15 * time.Minute
	}
	return c
}

type voiceWindow struct {
	end domain.StreamPos
	z   float64
}

// Voice turns VAD-classified audio frames into a z-score of the speech level:
// only speech is measured, which reduces false positives from explosions and
// game music; the level of a window is the power mean of its speech frames.
type Voice struct {
	cfg      VoiceConfig
	baseline *EWMA

	winStart  domain.StreamPos
	hasWin    bool
	frames    int
	speech    int
	powerSum  float64
	history   []voiceWindow
	lastFrame domain.StreamPos
}

var _ Detector = (*Voice)(nil)

// NewVoice builds a voice detector.
func NewVoice(cfg VoiceConfig) *Voice {
	cfg = cfg.withDefaults()
	return &Voice{cfg: cfg, baseline: NewEWMA(cfg.Alpha, cfg.MinStdDB)}
}

// Kind returns SignalVoice.
func (v *Voice) Kind() domain.SignalKind { return domain.SignalVoice }

// Advance is a no-op: the voice detector is driven by frames.
func (v *Voice) Advance(domain.StreamPos) {}

// Observe folds one audio frame. Frames must arrive in position order;
// out-of-order frames are dropped.
func (v *Voice) Observe(f domain.AudioFrame) {
	if v.hasWin && f.Pos < v.lastFrame {
		return
	}
	v.lastFrame = f.Pos
	if !v.hasWin {
		v.winStart, v.hasWin = f.Pos, true
	}
	for f.Pos >= v.winStart+domain.StreamPos(v.cfg.Window) {
		v.closeWindow()
	}
	v.frames++
	if f.Speech && finite(f.DBFS) {
		v.speech++
		v.powerSum += math.Pow(10, min(f.DBFS, 0)/10)
	}
}

func (v *Voice) closeWindow() {
	end := v.winStart + domain.StreamPos(v.cfg.Window)
	if v.frames > 0 && float64(v.speech)/float64(v.frames) >= v.cfg.MinSpeechRatio && v.powerSum > 0 {
		level := 10 * math.Log10(v.powerSum/float64(v.speech))
		z := v.baseline.Observe(level)
		v.history = append(v.history, voiceWindow{end: end, z: z})
		v.trim(end)
	}
	v.winStart = end
	v.frames, v.speech, v.powerSum = 0, 0, 0
}

func (v *Voice) trim(now domain.StreamPos) {
	cut := now - domain.StreamPos(v.cfg.Retention)
	i := sort.Search(len(v.history), func(i int) bool { return v.history[i].end >= cut })
	if i > 0 {
		v.history = append(v.history[:0], v.history[i:]...)
	}
}

// Value is the z-score of the latest speech window ending at or before pos,
// or 0 if that window is older than StaleAfter.
func (v *Voice) Value(pos domain.StreamPos) float64 {
	i := sort.Search(len(v.history), func(i int) bool { return v.history[i].end > pos })
	if i == 0 {
		return 0
	}
	w := v.history[i-1]
	if pos-w.end > domain.StreamPos(v.cfg.StaleAfter) {
		return 0
	}
	return w.z
}

// Baseline exposes the baseline (for metrics and tests).
func (v *Voice) Baseline() *EWMA { return v.baseline }
