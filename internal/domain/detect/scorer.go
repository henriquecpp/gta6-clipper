package detect

import (
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ScorerConfig configures the fusion of signals.
type ScorerConfig struct {
	Weights      map[domain.SignalKind]float64
	ThresholdOn  float64
	ThresholdOff float64
	ZMax         float64
	// MinSignalZ: at least one fast signal (voice or chat) must reach it.
	MinSignalZ float64
	// Warmup is the initial period without triggers while baselines learn.
	Warmup time.Duration
	// Cooldown is the minimum distance between consecutive peaks.
	Cooldown time.Duration
	// MaxEpisode closes an episode that stays above ThresholdOff this long.
	MaxEpisode time.Duration
}

// Scorer fuses detector values into score(t) = Σ wᵢ·clamp(zᵢ, 0, zmax) and
// emits one Moment per episode (hysteresis: armed again only after the score
// falls below ThresholdOff), at the best-scoring position of the episode.
type Scorer struct {
	cfg       ScorerConfig
	detectors []Detector

	armed      bool
	active     bool
	best       domain.Moment
	episodeAt  domain.StreamPos
	hasLast    bool
	lastPeak   domain.StreamPos
	lastScore  float64
	lastValues map[domain.SignalKind]float64
}

// NewScorer builds a scorer over the given detectors.
func NewScorer(cfg ScorerConfig, detectors ...Detector) *Scorer {
	if cfg.MaxEpisode <= 0 {
		cfg.MaxEpisode = 30 * time.Second
	}
	if cfg.ZMax <= 0 {
		cfg.ZMax = 6
	}
	return &Scorer{cfg: cfg, detectors: detectors, armed: true}
}

// Score computes the weighted score and the clamped value of every detector at pos.
func (s *Scorer) Score(pos domain.StreamPos) (float64, map[domain.SignalKind]float64) {
	values := make(map[domain.SignalKind]float64, len(s.detectors))
	score := 0.0
	for _, d := range s.detectors {
		v := clamp(d.Value(pos), 0, s.cfg.ZMax)
		values[d.Kind()] = v
		score += s.cfg.Weights[d.Kind()] * v
	}
	return score, values
}

// Evaluate advances every detector to pos, updates the episode state and
// returns a moment when an episode closes.
func (s *Scorer) Evaluate(pos domain.StreamPos) (domain.Moment, bool) {
	for _, d := range s.detectors {
		d.Advance(pos)
	}
	score, values := s.Score(pos)
	s.lastScore, s.lastValues = score, values

	if pos < domain.StreamPos(s.cfg.Warmup) {
		s.active = false
		s.armed = true
		return domain.Moment{}, false
	}
	gate := values[domain.SignalVoice] >= s.cfg.MinSignalZ || values[domain.SignalChat] >= s.cfg.MinSignalZ

	if !s.active {
		if score < s.cfg.ThresholdOff {
			s.armed = true
		}
		cooled := !s.hasLast || pos-s.lastPeak >= domain.StreamPos(s.cfg.Cooldown)
		if s.armed && cooled && gate && score >= s.cfg.ThresholdOn {
			s.active, s.armed = true, false
			s.episodeAt = pos
			s.best = domain.Moment{Peak: pos, Score: score, Values: values}
		}
		return domain.Moment{}, false
	}

	if gate && score > s.best.Score {
		s.best = domain.Moment{Peak: pos, Score: score, Values: values}
	}
	ended := score < s.cfg.ThresholdOff
	if ended || pos-s.episodeAt >= domain.StreamPos(s.cfg.MaxEpisode) {
		s.active = false
		s.armed = ended
		s.hasLast, s.lastPeak = true, s.best.Peak
		return s.best, true
	}
	return domain.Moment{}, false
}

// Last returns the score and values computed by the latest Evaluate.
func (s *Scorer) Last() (float64, map[domain.SignalKind]float64) { return s.lastScore, s.lastValues }

// LastPeak returns the peak of the latest emitted moment.
func (s *Scorer) LastPeak() (domain.StreamPos, bool) { return s.lastPeak, s.hasLast }

// NotePeak records an externally accepted peak (e.g. a late clip moment) so
// the cooldown also applies to it.
func (s *Scorer) NotePeak(p domain.StreamPos) {
	if !s.hasLast || p > s.lastPeak {
		s.hasLast, s.lastPeak = true, p
	}
}

// Weight returns the configured weight of a signal.
func (s *Scorer) Weight(k domain.SignalKind) float64 { return s.cfg.Weights[k] }

// ZMax returns the clamp ceiling.
func (s *Scorer) ZMax() float64 { return s.cfg.ZMax }
