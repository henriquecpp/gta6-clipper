package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func sec(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

var constraints = EditConstraints{
	WindowDuration: 90 * time.Second, Peak: 60 * time.Second,
	MinDuration: 15 * time.Second, MaxDuration: 60 * time.Second, PeakTolerance: 5 * time.Second,
	HeadlineMaxChars: 40, Streamer: "Streamer_A",
}

func validProposal() EditProposal {
	return EditProposal{
		StartS: 40, EndS: 70, Headline: "Ele pulou da ponte!",
		Captions: []ProposedCaption{{StartS: 41, EndS: 44, Text: "Vocês viram isso?"}, {StartS: 44, EndS: 48, Text: "Ele pulou da ponte"}},
		PostText: "Momento insano na live de streamer_a #gta6", Reasoning: "contexto antes do pulo",
	}
}

func TestNewEditPlanValid(t *testing.T) {
	plan, err := NewEditPlan(validProposal(), constraints)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Start != 40*time.Second || plan.End != 70*time.Second || plan.Duration() != 30*time.Second || len(plan.Captions) != 2 || plan.Captions[1].Start != 44*time.Second {
		t.Fatalf("plan = %+v", plan)
	}
}

// The streamer may say nothing in the cut (eating, watching a cutscene):
// no captions is a valid plan, and speech that merely contains parentheses
// is still speech.
func TestNewEditPlanWithoutSpeech(t *testing.T) {
	p := validProposal()
	p.Captions = nil
	plan, err := NewEditPlan(p, constraints)
	if err != nil || len(plan.Captions) != 0 {
		t.Fatalf("plan = %+v, err = %v", plan, err)
	}
	p = validProposal()
	p.Captions[0].Text = "(calma) eu vi isso"
	if _, err := NewEditPlan(p, constraints); err != nil {
		t.Fatalf("speech with parentheses rejected: %v", err)
	}
}

func TestNewEditPlanInvalid(t *testing.T) {
	cases := map[string]func(p *EditProposal){
		"end before start":   func(p *EditProposal) { p.StartS, p.EndS = 50, 45 },
		"past window":        func(p *EditProposal) { p.EndS = 95 },
		"negative start":     func(p *EditProposal) { p.StartS = -1 },
		"too short":          func(p *EditProposal) { p.StartS, p.EndS = 55, 65 },
		"too long":           func(p *EditProposal) { p.StartS, p.EndS = 10, 80 },
		"misses peak":        func(p *EditProposal) { p.StartS, p.EndS = 10, 40 },
		"empty headline":     func(p *EditProposal) { p.Headline = "  " },
		"long headline":      func(p *EditProposal) { p.Headline = strings.Repeat("á", 41) },
		"no credit":          func(p *EditProposal) { p.PostText = "Momento insano" },
		"empty post":         func(p *EditProposal) { p.PostText = "" },
		"caption outside":    func(p *EditProposal) { p.Captions[0].StartS = 35 },
		"stage direction":    func(p *EditProposal) { p.Captions[0].Text = "(comendo melancia na live)" },
		"sound effect":       func(p *EditProposal) { p.Captions[1].Text = " *grita* " },
		"bracketed":          func(p *EditProposal) { p.Captions[1].Text = "[risos]" },
		"caption overlap":    func(p *EditProposal) { p.Captions[1].StartS = 43 },
		"caption inverted":   func(p *EditProposal) { p.Captions[0].EndS = 41 },
		"caption empty text": func(p *EditProposal) { p.Captions[0].Text = "" },
		"nan":                func(p *EditProposal) { p.StartS = nan() },
	}
	for name, mutate := range cases {
		p := validProposal()
		mutate(&p)
		if _, err := NewEditPlan(p, constraints); !errors.Is(err, ErrInvalidEditPlan) || !IsInvalidEditPlan(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// All problems are reported together.
	p := validProposal()
	p.Headline, p.PostText = "", ""
	_, err := NewEditPlan(p, constraints)
	if !strings.Contains(err.Error(), "headline") || !strings.Contains(err.Error(), "post_text") {
		t.Fatalf("err = %v", err)
	}
}

func nan() float64 { z := 0.0; return z / z }

func TestSnapToWords(t *testing.T) {
	tr := Transcript{Words: []Word{
		{Text: "a", Start: sec(39.0), End: sec(39.5)},
		{Text: "vocês", Start: sec(39.8), End: sec(40.6)}, // the cut starts inside it
		{Text: "viram", Start: sec(40.7), End: sec(41.2)},
		{Text: "ponte", Start: sec(69.5), End: sec(70.4)}, // the cut ends inside it
		{Text: "kkk", Start: sec(70.5), End: sec(71.0)},
	}}
	plan, err := NewEditPlan(validProposal(), constraints)
	if err != nil {
		t.Fatal(err)
	}
	got := plan.SnapToWords(tr, 150*time.Millisecond, constraints)
	// Start 40.0 is inside "vocês" (39.8–40.6), nearest edge 39.8, minus margin but
	// not before "a" ends (39.5): 39.65. End 70.0 inside "ponte" (69.5–70.4):
	// nearest edge 70.4, plus margin but not past "kkk" (70.5): 70.5.
	if got.Start != sec(39.65) || got.End != sec(70.5) {
		t.Fatalf("snapped = %v–%v", got.Start, got.End)
	}
	if got.Captions[0].Start != sec(41) {
		t.Fatalf("captions = %+v", got.Captions)
	}
	// In silence: only the margin is applied.
	quiet := Transcript{Words: []Word{{Text: "x", Start: sec(50), End: sec(51)}}}
	got = plan.SnapToWords(quiet, 150*time.Millisecond, constraints)
	if got.Start != sec(39.85) || got.End != sec(70.15) {
		t.Fatalf("silence snap = %v–%v", got.Start, got.End)
	}
	// If snapping breaks the max duration, the original plan is kept.
	tight := constraints
	tight.MaxDuration = 30 * time.Second
	if got := plan.SnapToWords(tr, 150*time.Millisecond, tight); got.Start != plan.Start || got.End != plan.End {
		t.Fatalf("constraint-breaking snap applied: %v–%v", got.Start, got.End)
	}
}

func TestSnapClipsCaptions(t *testing.T) {
	plan := EditPlan{Start: sec(40), End: sec(70), Captions: []Caption{{Start: sec(40), End: sec(42), Text: "a"}, {Start: sec(69.9), End: sec(70), Text: "b"}}}
	tr := Transcript{Words: []Word{{Start: sec(40.1), End: sec(40.4)}, {Start: sec(69.0), End: sec(69.95)}}}
	// End 70 is after "69.0–69.95", start 40 before 40.1: margins only, end limited by window.
	got := plan.SnapToWords(tr, time.Second, constraints)
	if got.Start != sec(39) || got.End != sec(71) || len(got.Captions) != 2 {
		t.Fatalf("got %v–%v %+v", got.Start, got.End, got.Captions)
	}
	// Captions outside the cut are dropped.
	plan.Captions = append(plan.Captions, Caption{Start: sec(80), End: sec(81), Text: "c"})
	if got := plan.SnapToWords(tr, 0, constraints); len(got.Captions) != 2 {
		t.Fatalf("captions = %+v", got.Captions)
	}
}

func TestNewEditPlanSearchMetadata(t *testing.T) {
	p := validProposal()
	p.YouTubeTitle = "  alanzoka  foge da <polícia> no GTA 6 😂 "
	p.SearchTags = []string{"  GTA 6 Fuga  ", "gta 6 fuga", "#perseguicao", "a,b", strings.Repeat("x", 41), "", `"quoted"`}
	for range 20 {
		p.SearchTags = append(p.SearchTags, "extra")
	}
	plan, err := NewEditPlan(p, constraints)
	if err != nil {
		t.Fatal(err)
	}
	if plan.YouTubeTitle != "alanzoka foge da polícia no GTA 6 😂" {
		t.Errorf("youtube title = %q", plan.YouTubeTitle)
	}
	if want := []string{"GTA 6 Fuga", "perseguicao", "ab", "quoted", "extra"}; strings.Join(plan.SearchTags, "|") != strings.Join(want, "|") {
		t.Errorf("search tags = %q, want %q", plan.SearchTags, want)
	}
	// Bad optional metadata never fails the edit: a long title falls back.
	p.YouTubeTitle = strings.Repeat("título ", 20)
	if plan, err := NewEditPlan(p, constraints); err != nil || plan.YouTubeTitle != "" {
		t.Fatalf("long title: %q, %v", plan.YouTubeTitle, err)
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = fmt.Sprintf("tag %d", i)
	}
	p.SearchTags = many
	if plan, _ := NewEditPlan(p, constraints); len(plan.SearchTags) != MaxSearchTags {
		t.Fatalf("kept %d search tags", len(plan.SearchTags))
	}
}
