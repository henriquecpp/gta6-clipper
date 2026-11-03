package detect

import (
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// scripted is a Detector whose values are a function of the position.
type scripted struct {
	kind     domain.SignalKind
	f        func(sec int) float64
	advanced domain.StreamPos
}

func (s *scripted) Kind() domain.SignalKind          { return s.kind }
func (s *scripted) Advance(p domain.StreamPos)       { s.advanced = p }
func (s *scripted) Value(p domain.StreamPos) float64 { return s.f(int(time.Duration(p) / time.Second)) }

func cfg() ScorerConfig {
	return ScorerConfig{
		Weights:     map[domain.SignalKind]float64{domain.SignalVoice: 0.45, domain.SignalChat: 0.45, domain.SignalClips: 0.10},
		ThresholdOn: 3, ThresholdOff: 1.5, ZMax: 6, MinSignalZ: 2,
		Warmup: 10 * time.Second, Cooldown: 60 * time.Second, MaxEpisode: 30 * time.Second,
	}
}

// run evaluates once per second over [0, secs) and returns emitted moments.
func run(s *Scorer, secs int) []domain.Moment {
	var out []domain.Moment
	for i := range secs {
		if m, ok := s.Evaluate(at(time.Duration(i) * time.Second)); ok {
			out = append(out, m)
		}
	}
	return out
}

// bump returns a triangular peak of height h centred at c with half-width w.
func bump(c, w int, h float64) func(int) float64 {
	return func(sec int) float64 {
		d := sec - c
		if d < 0 {
			d = -d
		}
		if d >= w {
			return 0
		}
		return h * float64(w-d) / float64(w)
	}
}

func sum(fs ...func(int) float64) func(int) float64 {
	return func(s int) float64 {
		t := 0.0
		for _, f := range fs {
			t += f(s)
		}
		return t
	}
}

func TestScorerEmitsBestPeakOfEpisode(t *testing.T) {
	voice := &scripted{kind: domain.SignalVoice, f: bump(100, 10, 8)}
	chat := &scripted{kind: domain.SignalChat, f: bump(104, 10, 8)}
	s := NewScorer(cfg(), voice, chat)
	ms := run(s, 200)
	if len(ms) != 1 {
		t.Fatalf("moments = %+v", ms)
	}
	m := ms[0]
	if m.Peak < at(100*time.Second) || m.Peak > at(104*time.Second) {
		t.Fatalf("peak at %v, want between voice and chat peaks", m.Peak)
	}
	// Values are clamped to z_max = 6.
	if m.Values[domain.SignalVoice] > 6 || m.Values[domain.SignalChat] > 6 {
		t.Fatalf("values not clamped: %v", m.Values)
	}
	want := 0.45*m.Values[domain.SignalVoice] + 0.45*m.Values[domain.SignalChat]
	if diff := m.Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("score %v != Σ w·z %v", m.Score, want)
	}
	if voice.advanced != at(199*time.Second) {
		t.Fatal("detectors not advanced")
	}
	if p, ok := s.LastPeak(); !ok || p != m.Peak {
		t.Fatal("LastPeak")
	}
	if sc, vals := s.Last(); sc != 0 || len(vals) != 2 {
		t.Fatalf("Last = %v %v", sc, vals)
	}
}

func TestScorerWarmup(t *testing.T) {
	voice := &scripted{kind: domain.SignalVoice, f: bump(5, 4, 10)}
	chat := &scripted{kind: domain.SignalChat, f: bump(5, 4, 10)}
	if ms := run(NewScorer(cfg(), voice, chat), 60); len(ms) != 0 {
		t.Fatalf("triggered during warm-up: %+v", ms)
	}
}

func TestScorerCooldownAndDedup(t *testing.T) {
	// Two peaks 30 s apart: the second is inside the 60 s cooldown.
	// Height 6 = z_max, so each bump has a single maximum (no clamp plateau).
	f := sum(bump(100, 6, 6), bump(130, 6, 6), bump(200, 6, 6))
	s := NewScorer(cfg(), &scripted{kind: domain.SignalVoice, f: f}, &scripted{kind: domain.SignalChat, f: f})
	ms := run(s, 260)
	if len(ms) != 2 || ms[0].Peak != at(100*time.Second) || ms[1].Peak != at(200*time.Second) {
		t.Fatalf("moments = %+v", ms)
	}
}

func TestScorerHysteresis(t *testing.T) {
	// Score oscillates between 2 and 3.6: crosses T_on repeatedly but never
	// falls below T_off, so after the first episode (closed by MaxEpisode) it
	// must not re-arm.
	osc := func(sec int) float64 {
		if sec < 20 {
			return 0
		}
		if sec%2 == 0 {
			return 4
		}
		return 2.3
	}
	c := cfg()
	c.Cooldown = time.Second
	s := NewScorer(c, &scripted{kind: domain.SignalVoice, f: osc}, &scripted{kind: domain.SignalChat, f: osc})
	ms := run(s, 200)
	if len(ms) != 1 {
		t.Fatalf("hysteresis violated: %d moments", len(ms))
	}
}

func TestScorerGateNeedsFastSignal(t *testing.T) {
	// Only clips (weight 0.1 × 6 = 0.6) can never reach T_on; with a huge
	// weight it would, but the gate requires voice or chat ≥ min_signal_z.
	c := cfg()
	c.Weights[domain.SignalClips] = 1
	clips := &scripted{kind: domain.SignalClips, f: bump(100, 10, 6)}
	voice := &scripted{kind: domain.SignalVoice, f: func(int) float64 { return 1 }}
	if ms := run(NewScorer(c, voice, clips), 200); len(ms) != 0 {
		t.Fatalf("triggered without a fast signal: %+v", ms)
	}
}

func TestScorerNotePeakAppliesCooldown(t *testing.T) {
	f := bump(100, 6, 8)
	s := NewScorer(cfg(), &scripted{kind: domain.SignalVoice, f: f}, &scripted{kind: domain.SignalChat, f: f})
	s.NotePeak(at(80 * time.Second)) // e.g. a late clip moment
	s.NotePeak(at(10 * time.Second)) // older peaks never move it back
	if ms := run(s, 200); len(ms) != 0 {
		t.Fatalf("cooldown after NotePeak violated: %+v", ms)
	}
	if s.Weight(domain.SignalVoice) != 0.45 || s.ZMax() != 6 {
		t.Fatal("accessors")
	}
}

func TestScorerDefaults(t *testing.T) {
	s := NewScorer(ScorerConfig{})
	if s.cfg.MaxEpisode != 30*time.Second || s.cfg.ZMax != 6 {
		t.Fatalf("defaults = %+v", s.cfg)
	}
}
