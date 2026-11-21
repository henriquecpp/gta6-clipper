package app_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/domain/detect"
)

func TestDetectMomentCalibratesFromDecisions(t *testing.T) {
	ctx := context.Background()
	repo := fakes.NewClipRepo()
	save := func(id, streamer string, st domain.ClipState, dec domain.Decision, at time.Duration) {
		t.Helper()
		c := &domain.Clip{ID: domain.ClipID(id), Streamer: streamer, State: st, Decision: dec, CreatedAt: captureStart}
		if dec != "" {
			c.DecidedAt = captureStart.Add(at)
		}
		if err := repo.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// Saved out of order: the calibration replays them by decision time.
	save("c3", "streamer", domain.StateFailed, domain.DecisionApproved, 3*time.Minute)
	save("c1", "streamer", domain.StateDiscarded, domain.DecisionDiscarded, time.Minute)
	save("c4", "streamer", domain.StateDiscarded, domain.DecisionDiscarded, 4*time.Minute)
	save("c2", "streamer", domain.StateExpired, domain.DecisionExpired, 2*time.Minute)
	save("c0", "streamer", domain.StateDiscarded, domain.DecisionDiscarded, 0)
	save("x1", "other", domain.StateDelivered, domain.DecisionApproved, time.Minute)
	save("p1", "streamer", domain.StatePreviewSent, "", 0) // undecided: ignored

	cfg := detectionConfig()
	cfg.Calibration = detect.CalibrationConfig{Window: 10, StepUp: 0.25, StepDown: 0.25, Max: 1}
	d := app.NewDetectMoment(cfg, repo, func() (domain.ClipID, error) { return "n", nil }, fakes.NewClock(captureStart), discardLog())
	if err := d.Calibrate(ctx); err != nil {
		t.Fatal(err)
	}
	// d d e a d → 0.25 0.5 0.5 0.25 0.5; "other" only approved → 0 (not listed).
	if got, want := d.Strictness(), map[string]float64{"streamer": 0.5}; !maps.Equal(got, want) {
		t.Fatalf("Strictness after Calibrate = %v, want %v", got, want)
	}
	d.StartStream(testStreamer, testLive, captureStart) // a live of a calibrated streamer
	for range 3 {
		d.ObserveDecision("streamer", domain.DecisionDiscarded)
	}
	d.ObserveDecision("other", domain.DecisionApproved)
	d.ObserveDecision("other", "")
	if got, want := d.Strictness(), map[string]float64{"streamer": 1}; !maps.Equal(got, want) {
		t.Fatalf("Strictness after decisions = %v, want %v (capped at Max)", got, want)
	}
	d.ObserveDecision("streamer", domain.DecisionApproved)
	if got := d.Strictness()["streamer"]; got != 0.75 {
		t.Fatalf("after an approval = %v, want 0.75", got)
	}

	// Max 0 disables the calibration.
	cfg.Calibration.Max = 0
	off := app.NewDetectMoment(cfg, repo, func() (domain.ClipID, error) { return "n", nil }, fakes.NewClock(captureStart), discardLog())
	if err := off.Calibrate(ctx); err != nil {
		t.Fatal(err)
	}
	off.ObserveDecision("streamer", domain.DecisionDiscarded)
	if got := off.Strictness(); len(got) != 0 {
		t.Fatalf("disabled calibration produced %v", got)
	}
}

func TestDecisionsFeedTheCalibration(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	type seen struct {
		streamer string
		dec      domain.Decision
	}
	var got []seen
	e.dec = app.NewDecisions(e.repo, e.media, e.notifier, e.clock, discardLog(), ttl,
		app.WithDecisionObserver(func(s string, d domain.Decision) { got = append(got, seen{s, d}) }))
	e.previewSent(t, "a")
	e.previewSent(t, "d")
	e.previewSent(t, "x")
	if err := e.dec.ApproveClip(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.dec.DiscardClip(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	_ = e.dec.DiscardClip(ctx, "d") // duplicate click: no second decision (error expected and ignored)
	e.clock.Advance(ttl + time.Minute)
	if _, err := e.dec.ExpireClips(ctx); err != nil {
		t.Fatal(err)
	}
	want := []seen{{"streamer_a", domain.DecisionApproved}, {"streamer_a", domain.DecisionDiscarded}, {"streamer_a", domain.DecisionExpired}}
	if len(got) != len(want) {
		t.Fatalf("observed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("observed %v, want %v", got, want)
		}
	}
}
