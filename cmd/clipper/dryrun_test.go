package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func words(from, step, length time.Duration, n int) []domain.Word {
	out := make([]domain.Word, n)
	for i := range out {
		s := from + time.Duration(i)*step
		out[i] = domain.Word{Text: fmt.Sprintf("w%d", i), Start: s, End: s + length}
	}
	return out
}

func TestOfflineProposalIsAValidPlan(t *testing.T) {
	base := domain.EditConstraints{
		WindowDuration: 90 * time.Second, Peak: 60 * time.Second,
		MinDuration: 15 * time.Second, MaxDuration: 60 * time.Second, PeakTolerance: 5 * time.Second,
		HeadlineMaxChars: 60, Streamer: "caseoh_",
	}
	tests := []struct {
		name   string
		mod    func(*domain.EditConstraints)
		words  []domain.Word
		wantS  time.Duration
		wantE  time.Duration
		minCap int
	}{
		{name: "peak two thirds in", words: words(0, 400*time.Millisecond, 300*time.Millisecond, 220),
			wantS: 40 * time.Second, wantE: 70 * time.Second, minCap: 5},
		{name: "peak near the start", mod: func(c *domain.EditConstraints) { c.Peak = 5 * time.Second },
			words: words(0, time.Second, 800*time.Millisecond, 80), wantS: 0, wantE: 30 * time.Second, minCap: 5},
		{name: "peak at the end", mod: func(c *domain.EditConstraints) { c.Peak = 90 * time.Second },
			words: words(0, time.Second, 800*time.Millisecond, 90), wantS: 60 * time.Second, wantE: 90 * time.Second, minCap: 5},
		{name: "short window", mod: func(c *domain.EditConstraints) { c.WindowDuration, c.Peak = 20*time.Second, 15*time.Second },
			words: words(0, time.Second, 900*time.Millisecond, 20), wantS: 0, wantE: 20 * time.Second, minCap: 2},
		{name: "no speech", words: nil, wantS: 40 * time.Second, wantE: 70 * time.Second, minCap: 1},
		{name: "overlapping word times", words: []domain.Word{
			{Text: "a", Start: 50 * time.Second, End: 52 * time.Second},
			{Text: "b", Start: 51 * time.Second, End: 55 * time.Second},
			{Text: "c", Start: 54 * time.Second, End: 58 * time.Second},
			{Text: "d", Start: 54500 * time.Millisecond, End: 59 * time.Second},
		}, wantS: 40 * time.Second, wantE: 70 * time.Second, minCap: 2},
		{name: "long headline is truncated", mod: func(c *domain.EditConstraints) { c.HeadlineMaxChars = 10 },
			words: words(45*time.Second, time.Second, 500*time.Millisecond, 10), wantS: 40 * time.Second, wantE: 70 * time.Second, minCap: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			if tt.mod != nil {
				tt.mod(&c)
			}
			p := offlineProposal(domain.Transcript{Language: "en", Words: tt.words}, c)
			plan, err := domain.NewEditPlan(p, c)
			if err != nil {
				t.Fatalf("NewEditPlan: %v", err)
			}
			if plan.Start != tt.wantS || plan.End != tt.wantE {
				t.Errorf("cut = %v–%v, want %v–%v", plan.Start, plan.End, tt.wantS, tt.wantE)
			}
			if len(plan.Captions) < tt.minCap {
				t.Errorf("captions = %d, want >= %d", len(plan.Captions), tt.minCap)
			}
		})
	}
}
