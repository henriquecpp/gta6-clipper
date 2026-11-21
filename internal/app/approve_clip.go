package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Decisions groups what approving, discarding and expiring clips need.
type Decisions struct {
	repo     ClipRepository
	media    MediaCapture
	notifier Notifier
	clock    Clock
	log      *slog.Logger
	metrics  Metrics
	ttl      time.Duration
	// produce starts the final production of an approved clip (Phase 5).
	produce func(ctx context.Context, id domain.ClipID)
	// observe receives every decision (detection calibration).
	observe func(streamer string, d domain.Decision)
}

// DecisionsOption configures optional collaborators.
type DecisionsOption func(*Decisions)

// WithProducer sets what runs after an approval (ProduceFinalClip queue).
func WithProducer(f func(ctx context.Context, id domain.ClipID)) DecisionsOption {
	return func(d *Decisions) { d.produce = f }
}

// WithDecisionObserver sets who learns from every approval, discard and
// expiry (DetectMoment.ObserveDecision calibrates the detection).
func WithDecisionObserver(f func(streamer string, d domain.Decision)) DecisionsOption {
	return func(d *Decisions) { d.observe = f }
}

// WithDecisionMetrics sets the metrics sink.
func WithDecisionMetrics(m Metrics) DecisionsOption { return func(d *Decisions) { d.metrics = m } }

// NewDecisions builds the approve/discard/expire use cases.
func NewDecisions(repo ClipRepository, media MediaCapture, n Notifier, clock Clock, log *slog.Logger, ttl time.Duration, opts ...DecisionsOption) *Decisions {
	d := &Decisions{repo: repo, media: media, notifier: n, clock: clock, log: log, metrics: NopMetrics{}, ttl: ttl}
	for _, o := range opts {
		o(d)
	}
	return d
}

// ApproveClip is the human approval of a preview. Duplicate clicks get
// domain.ErrInvalidTransition; an approval after the TTL expires the clip and
// returns domain.ErrExpired.
func (d *Decisions) ApproveClip(ctx context.Context, id domain.ClipID) error {
	var clip *domain.Clip
	expired := false
	err := d.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		now := d.clock.Now()
		if c.IsExpired(now, d.ttl) {
			if err := c.Transition(domain.StateExpired, now); err != nil {
				return err
			}
			expired = true
		} else if err := c.Transition(domain.StateApproved, now); err != nil {
			return err
		}
		clip = c
		return repo.Save(ctx, c)
	})
	if err != nil {
		return fmt.Errorf("approve clip %s: %w", id, err)
	}
	if expired {
		d.afterFinal(ctx, clip, domain.StateExpired)
		return fmt.Errorf("approve clip %s: %w", id, domain.ErrExpired)
	}
	d.metrics.ClipState(domain.StateApproved)
	d.metrics.Decision(clip.Streamer, true)
	d.log.Info("clip approved", slog.String("clip_id", string(id)), slog.String("streamer", clip.Streamer))
	d.observeDecision(clip)
	d.update(ctx, clip)
	if d.produce != nil {
		d.produce(ctx, id)
	}
	return nil
}

// afterFinal deletes the retained media of a discarded/expired clip, records
// the decision and edits the preview message.
func (d *Decisions) afterFinal(ctx context.Context, c *domain.Clip, state domain.ClipState) {
	for _, ref := range []domain.MediaRef{c.SourceMedia, c.PreviewMedia} {
		if ref == "" {
			continue
		}
		if err := d.media.Delete(ctx, ref); err != nil {
			d.log.Warn("delete media", slog.String("clip_id", string(c.ID)), slog.String("err", err.Error()))
		}
	}
	d.metrics.ClipState(state)
	d.metrics.Decision(c.Streamer, false)
	d.log.Info("clip "+string(state), slog.String("clip_id", string(c.ID)), slog.String("streamer", c.Streamer))
	d.observeDecision(c)
	d.update(ctx, c)
}

func (d *Decisions) observeDecision(c *domain.Clip) {
	if d.observe != nil && c.Decision != "" {
		d.observe(c.Streamer, c.Decision)
	}
}

func (d *Decisions) update(ctx context.Context, c *domain.Clip) {
	if err := d.notifier.UpdatePreview(ctx, c); err != nil {
		d.log.Warn("update preview message", slog.String("clip_id", string(c.ID)), slog.String("err", err.Error()))
	}
}
