package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// PublishConfig configures the publication of final videos as Shorts.
type PublishConfig struct {
	// Upload holds the hashtags, tags and text language; the audio language
	// comes from the streamer.
	Upload domain.UploadOptions
	// Streamers gives the language spoken by each login.
	Streamers map[string]domain.Streamer
}

// RequestPublish is the human request to publish a delivered clip (button on
// the final video): delivered → publishing, then the upload is queued.
// Duplicate clicks get domain.ErrInvalidTransition, so a video is never
// uploaded twice by accident.
type RequestPublish struct {
	repo     ClipRepository
	notifier Notifier
	clock    Clock
	log      *slog.Logger
	metrics  Metrics
	submit   func(ctx context.Context, id domain.ClipID) bool
}

// NewRequestPublish builds the use case; submit queues PublishClip.Execute.
func NewRequestPublish(repo ClipRepository, n Notifier, clock Clock, log *slog.Logger, metrics Metrics,
	submit func(ctx context.Context, id domain.ClipID) bool) *RequestPublish {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	return &RequestPublish{repo: repo, notifier: n, clock: clock, log: log, metrics: metrics, submit: submit}
}

// Execute moves the clip to publishing and queues the upload.
func (r *RequestPublish) Execute(ctx context.Context, id domain.ClipID) error {
	var clip *domain.Clip
	err := r.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := c.Transition(domain.StatePublishing, r.clock.Now()); err != nil {
			return err
		}
		clip = c
		return repo.Save(ctx, c)
	})
	if err != nil {
		return fmt.Errorf("request publication of %s: %w", id, err)
	}
	r.metrics.ClipState(domain.StatePublishing)
	r.log.Info("publication requested", slog.String("clip_id", string(id)), slog.String("streamer", clip.Streamer))
	if r.submit(ctx, id) {
		if err := r.notifier.UpdateFinal(ctx, clip); err != nil {
			r.log.Warn("update final message", slog.String("clip_id", string(id)), slog.String("err", err.Error()))
		}
		return nil
	}
	// Queue full or shutting down: give the button back instead of leaving
	// the clip stuck in publishing.
	reason := "fila de publicação cheia ou em desligamento; tente de novo"
	if err := revertPublication(ctx, r.repo, r.notifier, r.clock, r.log, id, reason); err != nil {
		return fmt.Errorf("request publication of %s: %w", id, err)
	}
	return fmt.Errorf("request publication of %s: %s", id, reason)
}

// PublishClip uploads a clip in publishing (the job of the publish queue) and
// records where it was published; a failed upload goes back to delivered with
// the reason, and the human may ask again.
type PublishClip struct {
	cfg       PublishConfig
	repo      ClipRepository
	publisher Publisher
	notifier  Notifier
	clock     Clock
	log       *slog.Logger
	metrics   Metrics
	errors    *ErrorLog
}

// NewPublishClip builds the use case; metrics and errs may be nil.
func NewPublishClip(cfg PublishConfig, repo ClipRepository, p Publisher, n Notifier, clock Clock, log *slog.Logger,
	metrics Metrics, errs *ErrorLog) *PublishClip {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	if errs == nil {
		errs = &ErrorLog{}
	}
	return &PublishClip{cfg: cfg, repo: repo, publisher: p, notifier: n, clock: clock, log: log, metrics: metrics, errors: errs}
}

// Execute publishes one clip. Clips not in publishing are ignored (duplicate
// jobs are harmless).
func (u *PublishClip) Execute(ctx context.Context, id domain.ClipID) error {
	c, err := u.repo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("publish clip %s: %w", id, err)
	}
	if c.State != domain.StatePublishing {
		return nil
	}
	log := u.log.With(slog.String("clip_id", string(id)), slog.String("streamer", c.Streamer))
	if c.FinalMedia == "" {
		return u.failed(ctx, id, errors.New("clip has no final video"))
	}
	opts := u.cfg.Upload
	opts.AudioLanguage = u.cfg.Streamers[c.Streamer].Language
	meta := domain.BuildUploadMeta(c, opts)

	t0 := u.clock.Now()
	pub, err := u.publisher.Publish(ctx, c.FinalMedia, meta)
	u.metrics.StageDuration("publish", u.clock.Now().Sub(t0))
	if err != nil {
		return u.failed(ctx, id, err)
	}
	var out *domain.Clip
	err = u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		cur, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := cur.MarkPublished(pub, u.clock.Now()); err != nil {
			return err
		}
		out = cur
		return repo.Save(ctx, cur)
	})
	if err != nil {
		// The video is online but the state was not saved: the URL must not
		// be lost, or a second tap would publish it again.
		err = fmt.Errorf("record publication of %s (%s): %w", id, pub.URL, err)
		u.errors.Record(err, u.clock.Now())
		if aerr := u.notifier.SendAlert(ctx, c, "⚠️ Vídeo publicado, mas o estado não foi salvo: "+pub.URL); aerr != nil {
			log.Warn("send alert", slog.String("err", aerr.Error()))
		}
		return err
	}
	u.metrics.ClipState(domain.StatePublished)
	log.Info("clip published", slog.String("url", pub.URL), slog.String("title", meta.Title))
	if err := u.notifier.UpdateFinal(ctx, out); err != nil {
		log.Warn("update final message", slog.String("err", err.Error()))
	}
	return nil
}

// Interrupted handles a clip left in publishing by a restart: the upload may
// or may not have finished, so the clip goes back to delivered with a warning
// to check the channel before asking again (never re-uploaded automatically).
func (u *PublishClip) Interrupted(ctx context.Context, id domain.ClipID) error {
	return revertPublication(ctx, u.repo, u.notifier, u.clock, u.log, id,
		"envio interrompido por um reinício; confira o canal antes de publicar de novo")
}

func (u *PublishClip) failed(ctx context.Context, id domain.ClipID, cause error) error {
	u.errors.Record(fmt.Errorf("publish clip %s: %w", id, cause), u.clock.Now())
	if err := revertPublication(ctx, u.repo, u.notifier, u.clock, u.log, id, cause.Error()); err != nil {
		return errors.Join(fmt.Errorf("publish clip %s: %w", id, cause), err)
	}
	return fmt.Errorf("publish clip %s: %w", id, cause)
}

// revertPublication moves publishing → delivered with the reason and edits
// the final message (the publish button comes back).
func revertPublication(ctx context.Context, repo ClipRepository, n Notifier, clock Clock, log *slog.Logger,
	id domain.ClipID, reason string) error {
	var out *domain.Clip
	err := repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := c.PublishFailed(reason, clock.Now()); err != nil {
			return err
		}
		out = c
		return repo.Save(ctx, c)
	})
	if err != nil {
		return fmt.Errorf("revert publication of %s: %w", id, err)
	}
	log.Warn("publication not completed", slog.String("clip_id", string(id)), slog.String("reason", reason))
	if err := n.UpdateFinal(ctx, out); err != nil {
		log.Warn("update final message", slog.String("clip_id", string(id)), slog.String("err", err.Error()))
	}
	return nil
}
