package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ProduceConfig configures the final production.
type ProduceConfig struct {
	MinDuration      time.Duration
	MaxDuration      time.Duration
	PeakTolerance    time.Duration
	HeadlineMaxChars int
	OutputLanguage   string
	// SnapMargin is the silence kept before/after the words at the cut boundaries.
	SnapMargin time.Duration
	Pricing    domain.Pricing
	// MaxCost triggers an alert when an edit costs more (§12).
	MaxCost  domain.MicroUSD
	Width    int
	Height   int
	FPS      int
	MaxBytes int64
	// Streamers gives the layout of each login.
	Streamers map[string]domain.Streamer
}

// ProduceFinalClip turns an approved clip into the delivered vertical video:
// transcription → editor (only after approval: the AI cost rule) →
// validation with a single retry → word snapping → render → delivery.
type ProduceFinalClip struct {
	cfg         ProduceConfig
	repo        ClipRepository
	transcriber Transcriber
	editor      Editor
	renderer    Renderer
	notifier    Notifier
	media       MediaCapture
	clock       Clock
	log         *slog.Logger
	metrics     Metrics
	errors      *ErrorLog
}

// NewProduceFinalClip builds the use case; metrics and errs may be nil.
func NewProduceFinalClip(cfg ProduceConfig, repo ClipRepository, t Transcriber, e Editor, r Renderer, n Notifier, media MediaCapture,
	clock Clock, log *slog.Logger, metrics Metrics, errs *ErrorLog) *ProduceFinalClip {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	if errs == nil {
		errs = &ErrorLog{}
	}
	if cfg.OutputLanguage == "" {
		cfg.OutputLanguage = "pt-BR"
	}
	return &ProduceFinalClip{cfg: cfg, repo: repo, transcriber: t, editor: e, renderer: r, notifier: n, media: media,
		clock: clock, log: log, metrics: metrics, errors: errs}
}

// Execute runs (or resumes) the production of one clip. Clips that are not
// approved/editing/rendered are ignored, which makes duplicate jobs harmless.
func (u *ProduceFinalClip) Execute(ctx context.Context, id domain.ClipID) error {
	c, err := u.begin(ctx, id)
	if err != nil || c == nil {
		return err
	}
	log := u.log.With(slog.String("clip_id", string(id)), slog.String("streamer", c.Streamer))

	if c.State == domain.StateEditing {
		final, err := u.edit(ctx, c, log)
		if err != nil {
			return u.fail(ctx, id, err)
		}
		c = final
	}
	if c.State == domain.StateRendered {
		if err := u.deliver(ctx, c, log); err != nil {
			return u.fail(ctx, id, err)
		}
	}
	return nil
}

// begin moves approved → editing; editing and rendered are resumed as is.
func (u *ProduceFinalClip) begin(ctx context.Context, id domain.ClipID) (*domain.Clip, error) {
	var out *domain.Clip
	err := u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		switch c.State {
		case domain.StateApproved:
			if err := c.Transition(domain.StateEditing, u.clock.Now()); err != nil {
				return err
			}
			if err := repo.Save(ctx, c); err != nil {
				return err
			}
			u.metrics.ClipState(domain.StateEditing)
			out = c
		case domain.StateEditing, domain.StateRendered:
			out = c
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("start production of %s: %w", id, err)
	}
	return out, nil
}

// edit transcribes, plans (or reuses the saved plan) and renders; it returns
// the clip in the rendered state.
func (u *ProduceFinalClip) edit(ctx context.Context, c *domain.Clip, log *slog.Logger) (*domain.Clip, error) {
	t0 := u.clock.Now()
	transcript, err := u.transcriber.Transcribe(ctx, c.SourceMedia)
	u.metrics.StageDuration("transcribe", u.clock.Now().Sub(t0))
	if err != nil {
		return nil, fmt.Errorf("transcribe: %w", err)
	}
	cons := domain.EditConstraints{
		WindowDuration:   c.Window.Duration(),
		Peak:             time.Duration(c.Moment.Peak - c.Window.From),
		MinDuration:      u.cfg.MinDuration,
		MaxDuration:      u.cfg.MaxDuration,
		PeakTolerance:    u.cfg.PeakTolerance,
		HeadlineMaxChars: u.cfg.HeadlineMaxChars,
		Streamer:         c.Streamer,
	}

	plan := c.Plan
	if plan == nil {
		p, usage, err := u.plan(ctx, c, transcript, cons)
		cost := usage.Cost(u.cfg.Pricing)
		u.metrics.EditorUsage(cost, usage)
		if err != nil {
			u.saveUsage(ctx, c.ID, usage, cost)
			return nil, fmt.Errorf("edit: %w", err)
		}
		snapped := p.SnapToWords(transcript, u.cfg.SnapMargin, cons)
		plan = &snapped
		err = u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
			cur, err := repo.Get(ctx, c.ID)
			if err != nil {
				return err
			}
			cur.Plan, cur.Headline, cur.PostText = plan, plan.Headline, plan.PostText
			cur.Usage, cur.Cost = cur.Usage.Add(usage), cur.Cost+cost
			c = cur
			return repo.Save(ctx, cur)
		})
		if err != nil {
			return nil, fmt.Errorf("save edit plan: %w", err)
		}
		log.Info("edit plan ready", slog.Float64("start_s", plan.Start.Seconds()), slog.Float64("end_s", plan.End.Seconds()),
			slog.String("headline", plan.Headline), slog.String("cost", cost.String()), slog.Int64("input_tokens", usage.InputTokens),
			slog.Int64("output_tokens", usage.OutputTokens), slog.Int64("cache_read_tokens", usage.CacheReadTokens))
		if u.cfg.MaxCost > 0 && cost > u.cfg.MaxCost {
			msg := fmt.Sprintf("💸 Custo do Claude acima do limite: %s > %s", cost, u.cfg.MaxCost)
			if err := u.notifier.SendAlert(ctx, c, msg); err != nil {
				log.Warn("send cost alert", slog.String("err", err.Error()))
			}
		}
	}

	layout, stack := layoutOf(u.cfg.Streamers, c.Streamer)
	spec := domain.RenderSpec{
		ClipID: c.ID, Streamer: c.Streamer, Start: plan.Start, End: plan.End,
		Width: u.cfg.Width, Height: u.cfg.Height, FPS: u.cfg.FPS, Layout: layout, Stack: stack,
		Headline: plan.Headline, Captions: plan.Captions, Words: transcript.Between(plan.Start, plan.End), MaxBytes: u.cfg.MaxBytes,
	}
	t1 := u.clock.Now()
	video, err := u.renderer.Final(ctx, c.SourceMedia, spec)
	u.metrics.StageDuration("render", u.clock.Now().Sub(t1))
	if err != nil {
		return nil, fmt.Errorf("render final: %w", err)
	}
	var out *domain.Clip
	err = u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		cur, err := repo.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		cur.FinalMedia = video
		if err := cur.Transition(domain.StateRendered, u.clock.Now()); err != nil {
			return err
		}
		out = cur
		return repo.Save(ctx, cur)
	})
	if err != nil {
		return nil, fmt.Errorf("mark rendered: %w", err)
	}
	u.metrics.ClipState(domain.StateRendered)
	log.Info("final video rendered", slog.String("video", string(video)))
	return out, nil
}

// plan calls the editor and validates; an invalid answer gets exactly one
// retry with the problem sent back to the model.
func (u *ProduceFinalClip) plan(ctx context.Context, c *domain.Clip, t domain.Transcript, cons domain.EditConstraints) (domain.EditPlan, domain.Usage, error) {
	req := domain.EditRequest{
		ClipID: c.ID, Streamer: c.Streamer, Game: c.Game, StreamTitle: c.StreamTitle,
		Language: t.Language, OutputLanguage: u.cfg.OutputLanguage, Transcript: t,
		Signals: relativeSignals(c), ViewerClipTitles: c.Moment.ClipTitles, Constraints: cons,
	}
	var total domain.Usage
	for attempt := 0; attempt < 2; attempt++ {
		t0 := u.clock.Now()
		proposal, usage, err := u.editor.Plan(ctx, req)
		u.metrics.StageDuration("edit", u.clock.Now().Sub(t0))
		total = total.Add(usage)
		if err == nil {
			var plan domain.EditPlan
			plan, err = domain.NewEditPlan(proposal, cons)
			if err == nil {
				return plan, total, nil
			}
		}
		if !domain.IsInvalidEditPlan(err) || attempt == 1 {
			return domain.EditPlan{}, total, err
		}
		u.log.Warn("editor answer invalid; retrying once", slog.String("clip_id", string(c.ID)), slog.String("err", err.Error()))
		req.Retry = &domain.EditRetry{Previous: proposal, Problem: err.Error()}
	}
	return domain.EditPlan{}, total, errors.New("unreachable")
}

// relativeSignals converts the clip timeline to positions relative to the window.
func relativeSignals(c *domain.Clip) []domain.SignalPoint {
	out := make([]domain.SignalPoint, 0, len(c.Timeline))
	for _, p := range c.Timeline {
		p.Pos -= c.Window.From
		out = append(out, p)
	}
	return out
}

// saveUsage records the tokens spent on a failed edit (cost must be visible).
func (u *ProduceFinalClip) saveUsage(ctx context.Context, id domain.ClipID, usage domain.Usage, cost domain.MicroUSD) {
	if usage == (domain.Usage{}) {
		return
	}
	err := u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		cur, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		cur.Usage, cur.Cost = cur.Usage.Add(usage), cur.Cost+cost
		return repo.Save(ctx, cur)
	})
	if err != nil {
		u.log.Warn("save editor usage", slog.String("clip_id", string(id)), slog.String("err", err.Error()))
	}
}

// deliver sends the final video, marks the clip delivered and frees the
// pending media (the final video stays in the output directory).
func (u *ProduceFinalClip) deliver(ctx context.Context, c *domain.Clip, log *slog.Logger) error {
	t0 := u.clock.Now()
	post := domain.Post{Headline: c.Headline, Text: c.PostText}
	if err := u.notifier.SendFinal(ctx, c, c.FinalMedia, post); err != nil {
		return fmt.Errorf("deliver final video: %w", err)
	}
	u.metrics.StageDuration("deliver", u.clock.Now().Sub(t0))
	var out *domain.Clip
	err := u.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		cur, err := repo.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		if err := cur.Transition(domain.StateDelivered, u.clock.Now()); err != nil {
			return err
		}
		out = cur
		return repo.Save(ctx, cur)
	})
	if err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	u.metrics.ClipState(domain.StateDelivered)
	for _, ref := range []domain.MediaRef{out.SourceMedia, out.PreviewMedia} {
		if ref == "" {
			continue
		}
		if err := u.media.Delete(ctx, ref); err != nil {
			log.Warn("delete pending media", slog.String("err", err.Error()))
		}
	}
	if err := u.notifier.UpdatePreview(ctx, out); err != nil {
		log.Warn("update preview message", slog.String("err", err.Error()))
	}
	log.Info("clip delivered", slog.String("video", string(out.FinalMedia)), slog.String("cost", out.Cost.String()))
	return nil
}

func (u *ProduceFinalClip) fail(ctx context.Context, id domain.ClipID, cause error) error {
	u.errors.Record(fmt.Errorf("clip %s: %w", id, cause), u.clock.Now())
	err := failClip(ctx, u.repo, u.notifier, u.clock, u.log, u.metrics, id, cause)
	if c, gerr := u.repo.Get(ctx, id); gerr == nil {
		if uerr := u.notifier.UpdatePreview(ctx, c); uerr != nil {
			u.log.Warn("update preview message", slog.String("clip_id", string(id)), slog.String("err", uerr.Error()))
		}
	}
	return err
}

// layoutOf returns the vertical layout of a streamer; unknown streamers and
// invalid layouts fall back to blur.
func layoutOf(streamers map[string]domain.Streamer, login string) (domain.Layout, domain.StackRegions) {
	st := streamers[login]
	if !st.Layout.Valid() {
		return domain.LayoutBlur, st.Stack
	}
	return st.Layout, st.Stack
}
