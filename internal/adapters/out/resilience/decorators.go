package resilience

import (
	"context"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Default policies (docs/plans/fase-6.md).
var (
	NetworkPolicy = Policy{Attempts: 3, Base: time.Second, Max: 10 * time.Second, Jitter: 0.2}
	WhisperPolicy = Policy{Attempts: 3, Base: 2 * time.Second, Max: 20 * time.Second, Jitter: 0.2}
	EditorPolicy  = Policy{Attempts: 3, Base: 2 * time.Second, Max: 30 * time.Second, Jitter: 0.2}
	FFmpegPolicy  = Policy{Attempts: 2, Base: time.Second, Max: time.Second, Jitter: 0.2}
	// PublishPolicy never retries: an upload is not idempotent (a blind retry
	// could publish the video twice). The YouTube adapter resumes an
	// interrupted upload itself, and the human can ask again.
	PublishPolicy = Policy{Attempts: 1}
)

// Platform decorates app.Platform.
type Platform struct {
	Next   app.Platform
	Policy Policy
	deps   Deps
}

// NewPlatform wraps p.
func NewPlatform(p app.Platform, policy Policy, deps Deps) *Platform {
	return &Platform{Next: p, Policy: policy, deps: deps.withDefaults()}
}

// LiveStreams implements app.Platform.
func (d *Platform) LiveStreams(ctx context.Context, logins []string) ([]domain.LiveStream, error) {
	return call(ctx, d.deps, d.Policy, "platform", "live_streams", func(ctx context.Context) ([]domain.LiveStream, error) {
		return d.Next.LiveStreams(ctx, logins)
	})
}

// RecentClips implements app.Platform.
func (d *Platform) RecentClips(ctx context.Context, broadcasterID string, since time.Time) ([]domain.ViewerClip, error) {
	return call(ctx, d.deps, d.Policy, "platform", "recent_clips", func(ctx context.Context) ([]domain.ViewerClip, error) {
		return d.Next.RecentClips(ctx, broadcasterID, since)
	})
}

// MediaCapture decorates app.MediaCapture.
type MediaCapture struct {
	Next   app.MediaCapture
	Policy Policy
	deps   Deps
}

// NewMediaCapture wraps m.
func NewMediaCapture(m app.MediaCapture, policy Policy, deps Deps) *MediaCapture {
	return &MediaCapture{Next: m, Policy: policy, deps: deps.withDefaults()}
}

type retained struct {
	ref domain.MediaRef
	w   domain.Window
}

// RetainWindow implements app.MediaCapture.
func (d *MediaCapture) RetainWindow(ctx context.Context, id domain.StreamID, from, to domain.StreamPos) (domain.MediaRef, domain.Window, error) {
	r, err := call(ctx, d.deps, d.Policy, "capture", "retain_window", func(ctx context.Context) (retained, error) {
		ref, w, err := d.Next.RetainWindow(ctx, id, from, to)
		return retained{ref: ref, w: w}, err
	})
	return r.ref, r.w, err
}

// Delete implements app.MediaCapture.
func (d *MediaCapture) Delete(ctx context.Context, ref domain.MediaRef) error {
	return call0(ctx, d.deps, d.Policy, "capture", "delete", func(ctx context.Context) error { return d.Next.Delete(ctx, ref) })
}

// Renderer decorates app.Renderer.
type Renderer struct {
	Next   app.Renderer
	Policy Policy
	deps   Deps
}

// NewRenderer wraps r.
func NewRenderer(r app.Renderer, policy Policy, deps Deps) *Renderer {
	return &Renderer{Next: r, Policy: policy, deps: deps.withDefaults()}
}

// Preview implements app.Renderer.
func (d *Renderer) Preview(ctx context.Context, src domain.MediaRef, spec domain.PreviewSpec) (domain.MediaRef, error) {
	return call(ctx, d.deps, d.Policy, "renderer", "preview", func(ctx context.Context) (domain.MediaRef, error) {
		return d.Next.Preview(ctx, src, spec)
	})
}

// Final implements app.Renderer.
func (d *Renderer) Final(ctx context.Context, src domain.MediaRef, spec domain.RenderSpec) (domain.MediaRef, error) {
	return call(ctx, d.deps, d.Policy, "renderer", "final", func(ctx context.Context) (domain.MediaRef, error) {
		return d.Next.Final(ctx, src, spec)
	})
}

// Transcriber decorates app.Transcriber.
type Transcriber struct {
	Next   app.Transcriber
	Policy Policy
	deps   Deps
}

// NewTranscriber wraps t.
func NewTranscriber(t app.Transcriber, policy Policy, deps Deps) *Transcriber {
	return &Transcriber{Next: t, Policy: policy, deps: deps.withDefaults()}
}

// Transcribe implements app.Transcriber.
func (d *Transcriber) Transcribe(ctx context.Context, src domain.MediaRef) (domain.Transcript, error) {
	return call(ctx, d.deps, d.Policy, "transcriber", "transcribe", func(ctx context.Context) (domain.Transcript, error) {
		return d.Next.Transcribe(ctx, src)
	})
}

// Editor decorates app.Editor. Only API errors are retried (no tokens are
// charged for them); an invalid answer is returned at once, because the use
// case does its own single retry with the problem sent to the model.
type Editor struct {
	Next   app.Editor
	Policy Policy
	deps   Deps
}

// NewEditor wraps e.
func NewEditor(e app.Editor, policy Policy, deps Deps) *Editor {
	return &Editor{Next: e, Policy: policy, deps: deps.withDefaults()}
}

type planned struct {
	p domain.EditProposal
	u domain.Usage
}

// Plan implements app.Editor; usage of failed attempts is still reported.
func (d *Editor) Plan(ctx context.Context, req domain.EditRequest) (domain.EditProposal, domain.Usage, error) {
	var spent domain.Usage
	r, err := call(ctx, d.deps, d.Policy, "editor", "plan", func(ctx context.Context) (planned, error) {
		p, u, err := d.Next.Plan(ctx, req)
		spent = spent.Add(u)
		return planned{p: p, u: u}, err
	})
	return r.p, spent, err
}

// Notifier decorates app.Notifier.
type Notifier struct {
	Next   app.Notifier
	Policy Policy
	deps   Deps
}

// NewNotifier wraps n.
func NewNotifier(n app.Notifier, policy Policy, deps Deps) *Notifier {
	return &Notifier{Next: n, Policy: policy, deps: deps.withDefaults()}
}

// SendPreview implements app.Notifier.
func (d *Notifier) SendPreview(ctx context.Context, c *domain.Clip, preview domain.MediaRef) (domain.MessageRef, error) {
	return call(ctx, d.deps, d.Policy, "notifier", "send_preview", func(ctx context.Context) (domain.MessageRef, error) {
		return d.Next.SendPreview(ctx, c, preview)
	})
}

// UpdatePreview implements app.Notifier.
func (d *Notifier) UpdatePreview(ctx context.Context, c *domain.Clip) error {
	return call0(ctx, d.deps, d.Policy, "notifier", "update_preview", func(ctx context.Context) error { return d.Next.UpdatePreview(ctx, c) })
}

// SendFailure implements app.Notifier.
func (d *Notifier) SendFailure(ctx context.Context, c *domain.Clip, reason string) error {
	return call0(ctx, d.deps, d.Policy, "notifier", "send_failure", func(ctx context.Context) error { return d.Next.SendFailure(ctx, c, reason) })
}

// SendFinal implements app.Notifier.
func (d *Notifier) SendFinal(ctx context.Context, c *domain.Clip, video domain.MediaRef, post domain.Post) (domain.MessageRef, error) {
	return call(ctx, d.deps, d.Policy, "notifier", "send_final", func(ctx context.Context) (domain.MessageRef, error) {
		return d.Next.SendFinal(ctx, c, video, post)
	})
}

// UpdateFinal implements app.Notifier.
func (d *Notifier) UpdateFinal(ctx context.Context, c *domain.Clip) error {
	return call0(ctx, d.deps, d.Policy, "notifier", "update_final", func(ctx context.Context) error { return d.Next.UpdateFinal(ctx, c) })
}

// SendAlert implements app.Notifier.
func (d *Notifier) SendAlert(ctx context.Context, c *domain.Clip, text string) error {
	return call0(ctx, d.deps, d.Policy, "notifier", "send_alert", func(ctx context.Context) error { return d.Next.SendAlert(ctx, c, text) })
}

// Publisher decorates app.Publisher (use PublishPolicy: no retries).
type Publisher struct {
	Next   app.Publisher
	Policy Policy
	deps   Deps
}

// NewPublisher wraps p.
func NewPublisher(p app.Publisher, policy Policy, deps Deps) *Publisher {
	return &Publisher{Next: p, Policy: policy, deps: deps.withDefaults()}
}

// Publish implements app.Publisher.
func (d *Publisher) Publish(ctx context.Context, video domain.MediaRef, meta domain.UploadMeta) (domain.Publication, error) {
	return call(ctx, d.deps, d.Policy, "publisher", "publish", func(ctx context.Context) (domain.Publication, error) {
		return d.Next.Publish(ctx, video, meta)
	})
}

var (
	_ app.Publisher    = (*Publisher)(nil)
	_ app.Platform     = (*Platform)(nil)
	_ app.MediaCapture = (*MediaCapture)(nil)
	_ app.Renderer     = (*Renderer)(nil)
	_ app.Transcriber  = (*Transcriber)(nil)
	_ app.Editor       = (*Editor)(nil)
	_ app.Notifier     = (*Notifier)(nil)
)
