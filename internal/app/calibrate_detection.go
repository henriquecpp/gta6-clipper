package app

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// decidedStates are the states a clip can be in after a human decision.
var decidedStates = []domain.ClipState{
	domain.StateApproved, domain.StateEditing, domain.StateRendered, domain.StateDelivered,
	domain.StatePublishing, domain.StatePublished, domain.StateFailed, domain.StateDiscarded, domain.StateExpired,
}

// Calibrate loads the past decisions from the repository and sets the
// strictness offset of every streamer (run once at startup; ObserveDecision
// keeps it up to date afterwards).
func (d *DetectMoment) Calibrate(ctx context.Context) error {
	if d.cfg.Calibration.Max <= 0 {
		return nil
	}
	clips, err := d.repo.ListByState(ctx, decidedStates...)
	if err != nil {
		return fmt.Errorf("calibrate detection: %w", err)
	}
	clips = slices.DeleteFunc(clips, func(c *domain.Clip) bool { return c.Decision == "" })
	slices.SortStableFunc(clips, func(a, b *domain.Clip) int { return a.DecidedAt.Compare(b.DecidedAt) })
	history := make(map[string][]domain.Decision)
	for _, c := range clips {
		history[c.Streamer] = append(history[c.Streamer], c.Decision)
	}
	for _, login := range slices.Sorted(maps.Keys(history)) {
		d.updateDecisions(login, "loaded", func([]domain.Decision) []domain.Decision { return history[login] })
	}
	return nil
}

// ObserveDecision records an approval, discard or expiry of a streamer's
// clip and adjusts that streamer's detection strictness.
func (d *DetectMoment) ObserveDecision(streamer string, dec domain.Decision) {
	if d.cfg.Calibration.Max <= 0 || dec == "" {
		return
	}
	d.updateDecisions(streamer, "decision", func(h []domain.Decision) []domain.Decision { return append(h, dec) })
}

// Strictness returns the non-zero strictness offsets by streamer (for /status).
func (d *DetectMoment) Strictness() map[string]float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]float64, len(d.offsets))
	for k, v := range d.offsets {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

// updateDecisions changes the decision history of a streamer (atomically, so
// concurrent decisions are not lost), trims it to the calibration window,
// recomputes the offset and applies it to the streamer's open sessions.
func (d *DetectMoment) updateDecisions(streamer, why string, change func([]domain.Decision) []domain.Decision) {
	d.mu.Lock()
	history := change(slices.Clone(d.decisions[streamer]))
	if w := d.cfg.Calibration.Window; w > 0 && len(history) > w {
		history = history[len(history)-w:]
	}
	off := d.cfg.Calibration.Offset(history)
	prev := d.offsets[streamer]
	d.decisions[streamer], d.offsets[streamer] = history, off
	// Lock order d.mu → s.mu (no path takes them the other way round).
	for _, s := range d.sessions {
		if s.streamer.Login == streamer {
			s.mu.Lock()
			s.setStrictness(off)
			s.mu.Unlock()
		}
	}
	d.mu.Unlock()
	if off != prev || why == "loaded" {
		approved, discarded := 0, 0
		for _, dec := range history {
			switch dec {
			case domain.DecisionApproved:
				approved++
			case domain.DecisionDiscarded:
				discarded++
			}
		}
		d.log.Info("detection strictness calibrated", slog.String("streamer", streamer), slog.String("source", why),
			slog.Float64("offset", round2(off)), slog.Int("approved", approved), slog.Int("discarded", discarded))
	}
}

// setStrictness applies the calibration offset to the session's detectors (caller holds s.mu
// or owns the session before it is shared).
func (s *session) setStrictness(off float64) {
	s.scorer.SetStrictness(off)
	s.clips.SetStrictness(off)
}
