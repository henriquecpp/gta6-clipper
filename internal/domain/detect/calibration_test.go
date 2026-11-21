package detect

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func TestCalibrationOffset(t *testing.T) {
	const (
		a = domain.DecisionApproved
		d = domain.DecisionDiscarded
		e = domain.DecisionExpired
	)
	c := CalibrationConfig{Window: 6, StepUp: 0.25, StepDown: 0.25, Max: 1}
	tests := []struct {
		name string
		cfg  CalibrationConfig
		in   []domain.Decision
		want float64
	}{
		{"no decisions", c, nil, 0},
		{"each discard raises the bar", c, []domain.Decision{d, d, d}, 0.75},
		{"capped at Max", c, []domain.Decision{d, d, d, d, d, d}, 1},
		{"an approval lowers it", c, []domain.Decision{d, d, d, a}, 0.5},
		{"never below zero", c, []domain.Decision{a, a, a}, 0},
		{"old approvals are no credit against new discards", c, []domain.Decision{a, a, a, d, d}, 0.5},
		{"expired is neutral", c, []domain.Decision{d, e, e, d}, 0.5},
		{"only the last Window decisions count", c, []domain.Decision{d, d, d, d, a, a, a, a, a, a}, 0},
		{"Max 0 disables", CalibrationConfig{Window: 6, StepUp: 1, StepDown: 1}, []domain.Decision{d, d}, 0},
		{"faster to relax than to tighten", CalibrationConfig{Window: 10, StepUp: 0.25, StepDown: 0.5, Max: 2}, []domain.Decision{d, d, d, d, a}, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Offset(tt.in); math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("Offset = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStrictnessRaisesTheScorerBar(t *testing.T) {
	// voice and chat peak at z 3.6: score 3.24 ≥ threshold_on 3, both ≥ min_signal_z 2.
	newScorer := func(off float64) *Scorer {
		voice := &scripted{kind: domain.SignalVoice, f: bump(100, 10, 3.6)}
		chat := &scripted{kind: domain.SignalChat, f: bump(100, 10, 3.6)}
		s := NewScorer(cfg(), voice, chat)
		s.SetStrictness(off)
		return s
	}
	if ms := run(newScorer(0), 200); len(ms) != 1 {
		t.Fatalf("without strictness: %d moments, want 1", len(ms))
	}
	if ms := run(newScorer(0.5), 200); len(ms) != 0 {
		t.Fatalf("with strictness 0.5 (threshold 3.5): %d moments, want 0", len(ms))
	}
	s := newScorer(-1) // negative offsets are ignored
	if s.MinSignalZ() != 2 {
		t.Fatalf("MinSignalZ = %v, want 2", s.MinSignalZ())
	}
	s.SetStrictness(1.5)
	if s.MinSignalZ() != 3.5 {
		t.Fatalf("MinSignalZ with strictness = %v, want 3.5", s.MinSignalZ())
	}
}

func TestStrictnessRaisesTheClipRainBar(t *testing.T) {
	c := NewClips(ClipsConfig{GroupWindow: 15 * time.Second, MinClips: 3, MinZ: 3.5})
	for i := range 40 { // 4 clips/min over 10 min: λ = 2
		c.Observe(fmt.Sprintf("c%d", i), at(time.Duration(i)*15*time.Second))
	}
	now := at(10 * time.Minute)
	if got := c.Required(now); got != 7 {
		t.Fatalf("Required = %d, want 7", got)
	}
	c.SetStrictness(1.5) // z 5: ⌈2 + 5·1.41⌉ = 10
	if got := c.Required(now); got != 10 {
		t.Fatalf("Required with strictness = %d, want 10", got)
	}
}
