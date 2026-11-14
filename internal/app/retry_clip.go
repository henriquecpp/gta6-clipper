package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// RetryClip is the manual retry (/retry or the button): failed → approved,
// then the clip goes back to the production queue. A saved edit plan is
// reused, so the editor is not paid twice.
type RetryClip struct {
	repo     ClipRepository
	notifier Notifier
	clock    Clock
	log      *slog.Logger
	metrics  Metrics
	submit   func(ctx context.Context, id domain.ClipID) bool
}

// NewRetryClip builds the use case.
func NewRetryClip(repo ClipRepository, n Notifier, clock Clock, log *slog.Logger, metrics Metrics, submit func(ctx context.Context, id domain.ClipID) bool) *RetryClip {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	return &RetryClip{repo: repo, notifier: n, clock: clock, log: log, metrics: metrics, submit: submit}
}

// Execute retries a failed clip; other states return domain.ErrInvalidTransition.
func (r *RetryClip) Execute(ctx context.Context, id domain.ClipID) error {
	var clip *domain.Clip
	err := r.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := c.Transition(domain.StateApproved, r.clock.Now()); err != nil {
			return err
		}
		clip = c
		return repo.Save(ctx, c)
	})
	if err != nil {
		return fmt.Errorf("retry clip %s: %w", id, err)
	}
	r.metrics.ClipState(domain.StateApproved)
	r.log.Info("clip retried", slog.String("clip_id", string(id)), slog.String("streamer", clip.Streamer))
	if err := r.notifier.UpdatePreview(ctx, clip); err != nil {
		r.log.Warn("update preview message", slog.String("clip_id", string(id)), slog.String("err", err.Error()))
	}
	r.submit(ctx, id)
	return nil
}
