package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// PreviewConfig configures previews.
type PreviewConfig struct {
	Duration time.Duration
	Height   int
	MaxBytes int64
}

// ErrorLog remembers the last error for /status.
type ErrorLog struct {
	mu  sync.Mutex
	msg string
	at  time.Time
}

// Record stores err as the latest error.
func (l *ErrorLog) Record(err error, at time.Time) {
	if err == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msg, l.at = err.Error(), at
}

// Last returns the latest error and when it happened.
func (l *ErrorLog) Last() (string, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.msg, l.at
}

// SendPreview renders the preview of a detected clip, sends it for approval
// and moves the clip to preview_sent (or failed).
type SendPreview struct {
	cfg      PreviewConfig
	repo     ClipRepository
	renderer Renderer
	notifier Notifier
	clock    Clock
	log      *slog.Logger
	metrics  Metrics
	errors   *ErrorLog
}

// NewSendPreview builds the use case. metrics and errs may be nil.
func NewSendPreview(cfg PreviewConfig, repo ClipRepository, r Renderer, n Notifier, clock Clock, log *slog.Logger, metrics Metrics, errs *ErrorLog) *SendPreview {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	if errs == nil {
		errs = &ErrorLog{}
	}
	return &SendPreview{cfg: cfg, repo: repo, renderer: r, notifier: n, clock: clock, log: log, metrics: metrics, errors: errs}
}

// Execute processes one clip. A clip that is no longer detected is skipped
// (already sent, e.g. on a duplicate call after a restart).
func (u *SendPreview) Execute(ctx context.Context, id domain.ClipID) error {
	c, err := u.repo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("send preview of %s: %w", id, err)
	}
	if c.State != domain.StateDetected {
		return nil
	}
	start, length := domain.PreviewWindow(c.Window, c.Moment.Peak, u.cfg.Duration)
	spec := domain.PreviewSpec{Start: start, Duration: length, Height: u.cfg.Height, MaxBytes: u.cfg.MaxBytes}

	t0 := u.clock.Now()
	preview, err := u.renderer.Preview(ctx, c.SourceMedia, spec)
	u.metrics.StageDuration("preview", u.clock.Now().Sub(t0))
	if err != nil {
		return u.fail(ctx, c, fmt.Errorf("render preview: %w", err))
	}
	t1 := u.clock.Now()
	msg, err := u.notifier.SendPreview(ctx, c, preview)
	u.metrics.StageDuration("deliver", u.clock.Now().Sub(t1))
	if err != nil {
		return u.fail(ctx, c, fmt.Errorf("send preview: %w", err))
	}

	var saved *domain.Clip
	err = u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		cur, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		cur.PreviewMedia, cur.PreviewMessage = preview, msg
		if err := cur.Transition(domain.StatePreviewSent, u.clock.Now()); err != nil {
			return err
		}
		saved = cur
		return repo.Save(ctx, cur)
	})
	if err != nil {
		return fmt.Errorf("mark preview sent for %s: %w", id, err)
	}
	u.metrics.ClipState(domain.StatePreviewSent)
	if !saved.PeakAt.IsZero() {
		u.metrics.PreviewLatency(u.clock.Now().Sub(saved.PeakAt))
	}
	u.log.Info("preview sent", slog.String("clip_id", string(id)), slog.String("streamer", saved.Streamer),
		slog.Int64("duration_ms", u.clock.Now().Sub(t0).Milliseconds()))
	return nil
}

// fail records the failure on the clip and alerts the approver.
func (u *SendPreview) fail(ctx context.Context, c *domain.Clip, cause error) error {
	u.errors.Record(fmt.Errorf("clip %s: %w", c.ID, cause), u.clock.Now())
	return failClip(ctx, u.repo, u.notifier, u.clock, u.log, u.metrics, c.ID, cause)
}

// failClip moves a clip to failed (inside a transaction) and alerts.
func failClip(ctx context.Context, repo ClipRepository, n Notifier, clock Clock, log *slog.Logger, metrics Metrics, id domain.ClipID, cause error) error {
	var failed *domain.Clip
	txErr := repo.InTx(ctx, func(ctx context.Context, tx ClipRepository) error {
		cur, err := tx.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := cur.Fail(cause.Error(), clock.Now()); err != nil {
			return err
		}
		failed = cur
		return tx.Save(ctx, cur)
	})
	if txErr != nil {
		return errors.Join(cause, fmt.Errorf("mark clip %s failed: %w", id, txErr))
	}
	metrics.ClipState(domain.StateFailed)
	log.Error("clip failed", slog.String("clip_id", string(id)), slog.String("stage", string(failed.FailedFrom)), slog.String("err", cause.Error()))
	if err := n.SendFailure(ctx, failed, cause.Error()); err != nil {
		log.Warn("send failure alert", slog.String("clip_id", string(id)), slog.String("err", err.Error()))
	}
	return cause
}
