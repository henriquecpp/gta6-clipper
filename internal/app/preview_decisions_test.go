package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/contracttest"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

var t0 = time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)

type previewEnv struct {
	repo     *fakes.ClipRepo
	media    *fakes.MediaCapture
	renderer *fakes.Renderer
	notifier *fakes.Notifier
	clock    *fakes.Clock
	metrics  *fakes.Metrics
	errs     *app.ErrorLog
	send     *app.SendPreview
	dec      *app.Decisions
	produced []domain.ClipID
}

const ttl = 6 * time.Hour

func newPreviewEnv(t *testing.T) *previewEnv {
	t.Helper()
	e := &previewEnv{repo: fakes.NewClipRepo(), media: fakes.NewMediaCapture(), notifier: &fakes.Notifier{},
		clock: fakes.NewClock(t0), metrics: fakes.NewMetrics(), errs: &app.ErrorLog{}}
	e.renderer = &fakes.Renderer{Media: e.media}
	stack := domain.StackRegions{Facecam: domain.Rect{X: 0.7, W: 0.3, H: 0.3}, Gameplay: domain.Rect{W: 1, H: 1}}
	e.send = app.NewSendPreview(app.PreviewConfig{Duration: 40 * time.Second, Before: 25 * time.Second, Width: 480, Height: 854, FPS: 30, MaxBytes: 45 << 20,
		Streamers: map[string]domain.Streamer{"streamer_a": {Login: "streamer_a", Layout: domain.LayoutStack, Stack: stack}}},
		e.repo, e.renderer, e.notifier, e.clock, discardLog(), e.metrics, e.errs)
	e.dec = app.NewDecisions(e.repo, e.media, e.notifier, e.clock, discardLog(), ttl,
		app.WithDecisionMetrics(e.metrics),
		app.WithProducer(func(_ context.Context, id domain.ClipID) { e.produced = append(e.produced, id) }))
	return e
}

// detected stores a detected clip whose peak happened 20 s before t0.
func (e *previewEnv) detected(t *testing.T, id domain.ClipID) *domain.Clip {
	t.Helper()
	c := contracttest.FullClip(id, t0)
	c.PeakAt = t0.Add(-20 * time.Second)
	e.media.Add(c.SourceMedia)
	if err := e.repo.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *previewEnv) get(t *testing.T, id domain.ClipID) *domain.Clip {
	t.Helper()
	c, err := e.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSendPreviewHappyPath(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	c := e.detected(t, "c1")
	e.clock.Advance(5 * time.Second)
	if err := e.send.Execute(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	got := e.get(t, c.ID)
	if got.State != domain.StatePreviewSent || got.PreviewMessage != "1:1" || !e.media.IsLive(got.PreviewMedia) {
		t.Fatalf("clip = %+v", got)
	}
	spec := e.renderer.Previews[0]
	// Window 35–125 s, peak 95 s (60 s in): 25 s before the peak, 15 s of reaction after it.
	if spec.Duration != 40*time.Second || spec.Start != 35*time.Second {
		t.Fatalf("spec = %+v", spec)
	}
	// Vertical, with the streamer's layout: the preview looks like the final short.
	if spec.Width != 480 || spec.Height != 854 || spec.FPS != 30 || spec.Layout != domain.LayoutStack || spec.Stack.Facecam.X != 0.7 {
		t.Fatalf("preview frame = %+v", spec)
	}
	if len(e.metrics.Latency) != 1 || e.metrics.Latency[0] != 25*time.Second {
		t.Fatalf("preview latency = %v, want 25 s (peak → preview)", e.metrics.Latency)
	}
	// Idempotent: a second call does nothing.
	if err := e.send.Execute(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if sent, _, _ := e.notifier.Snapshot(); len(sent) != 1 {
		t.Fatalf("preview sent %d times", len(sent))
	}
}

func TestSendPreviewFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("nvenc unavailable")

	e := newPreviewEnv(t)
	e.renderer.PreviewErr = boom
	e.detected(t, "c1")
	if err := e.send.Execute(ctx, "c1"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	got := e.get(t, "c1")
	if got.State != domain.StateFailed || got.FailedFrom != domain.StateDetected {
		t.Fatalf("clip = %s from %s", got.State, got.FailedFrom)
	}
	if _, _, failures := e.notifier.Snapshot(); len(failures) != 1 {
		t.Fatalf("failure alerts = %v", failures)
	}
	if msg, _ := e.errs.Last(); msg == "" {
		t.Fatal("last error not recorded")
	}

	// Telegram timeout.
	e = newPreviewEnv(t)
	e.notifier.SendErr = context.DeadlineExceeded
	e.detected(t, "c2")
	if err := e.send.Execute(ctx, "c2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if e.get(t, "c2").State != domain.StateFailed {
		t.Fatal("clip should be failed after a Telegram timeout")
	}

	if err := e.send.Execute(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing clip err = %v", err)
	}
}

func (e *previewEnv) previewSent(t *testing.T, id domain.ClipID) {
	t.Helper()
	e.detected(t, id)
	if err := e.send.Execute(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestApproveIsIdempotent(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	e.previewSent(t, "c1")
	if err := e.dec.ApproveClip(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := e.dec.ApproveClip(ctx, "c1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("double click err = %v", err)
	}
	if err := e.dec.DiscardClip(ctx, "c1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("discard after approve err = %v", err)
	}
	if got := e.get(t, "c1"); got.State != domain.StateApproved || got.Decision != domain.DecisionApproved {
		t.Fatalf("clip = %s %s", got.State, got.Decision)
	}
	if len(e.produced) != 1 {
		t.Fatalf("production started %d times", len(e.produced))
	}
	if _, updates, _ := e.notifier.Snapshot(); len(updates) != 1 || updates[0] != domain.StateApproved {
		t.Fatalf("message updates = %v", updates)
	}
	if e.metrics.Approved["streamer_a"] != 1 || e.metrics.Decided["streamer_a"] != 1 {
		t.Fatalf("decision metrics = %+v", e.metrics)
	}
	if err := e.dec.ApproveClip(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown clip err = %v", err)
	}
}

func TestDiscardDeletesMedia(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	e.previewSent(t, "c1")
	c := e.get(t, "c1")
	if err := e.dec.DiscardClip(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if e.media.IsLive(c.SourceMedia) || e.media.IsLive(c.PreviewMedia) {
		t.Fatal("media not deleted on discard")
	}
	if got := e.get(t, "c1"); got.State != domain.StateDiscarded {
		t.Fatalf("state = %s", got.State)
	}
}

func TestApprovalAfterExpiration(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	e.previewSent(t, "c1")
	e.clock.Advance(ttl + time.Second)
	if err := e.dec.ApproveClip(ctx, "c1"); !errors.Is(err, domain.ErrExpired) {
		t.Fatalf("err = %v", err)
	}
	got := e.get(t, "c1")
	if got.State != domain.StateExpired || e.media.IsLive(got.SourceMedia) || len(e.produced) != 0 {
		t.Fatalf("clip = %s, media live %v, produced %v", got.State, e.media.IsLive(got.SourceMedia), e.produced)
	}
}

func TestExpireClips(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	e.previewSent(t, "old")
	e.clock.Advance(3 * time.Hour)
	e.previewSent(t, "new")
	e.clock.Advance(3*time.Hour + time.Minute)
	n, err := e.dec.ExpireClips(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired = %d, %v", n, err)
	}
	if e.get(t, "old").State != domain.StateExpired || e.get(t, "new").State != domain.StatePreviewSent {
		t.Fatal("wrong clip expired")
	}
	if err := e.dec.ExpireTask(ctx); err != nil {
		t.Fatal(err)
	}
	if _, updates, _ := e.notifier.Snapshot(); updates[len(updates)-1] != domain.StateExpired {
		t.Fatalf("updates = %v", updates)
	}
}

func TestStatusAndPending(t *testing.T) {
	e := newPreviewEnv(t)
	ctx := context.Background()
	e.previewSent(t, "a")
	e.previewSent(t, "b")
	_ = e.dec.ApproveClip(ctx, "b")
	c := e.get(t, "b")
	_ = c.Transition(domain.StateEditing, t0.Add(time.Minute))
	c.Cost = 1234
	_ = e.repo.Save(ctx, c)
	e.errs.Record(errors.New("boom"), t0)

	st := app.NewStatus(e.repo, e.clock, ttl, func() []domain.LiveStream { return []domain.LiveStream{{Login: "x"}} }, nil, e.errs)
	st.SetQueue(func() int { return 2 })
	st.SetOffGame(func() []domain.LiveStream { return []domain.LiveStream{{Login: "y", Game: "Just Chatting"}} })
	r, err := st.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pending != 1 || r.Queue != 2 || r.CostToday != 1234 || len(r.Lives) != 1 || len(r.OffGame) != 1 || r.LastError != "boom" || r.ClipsToday[domain.StateEditing] != 1 {
		t.Fatalf("report = %+v", r)
	}
	p, err := st.Pending(ctx)
	if err != nil || len(p) != 1 || p[0].Clip.ID != "a" || !p[0].Deadline.Equal(t0.Add(ttl)) {
		t.Fatalf("pending = %+v, %v", p, err)
	}
	var none app.ErrorLog
	none.Record(nil, t0)
	if msg, _ := none.Last(); msg != "" {
		t.Fatal("nil error recorded")
	}
}

func TestSendPreviewUnknownStreamerUsesBlur(t *testing.T) {
	e := newPreviewEnv(t)
	c := contracttest.FullClip("c9", t0)
	c.Streamer = "not_in_config"
	c.PeakAt = t0.Add(-20 * time.Second)
	e.media.Add(c.SourceMedia)
	if err := e.repo.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := e.send.Execute(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.renderer.Previews[0].Layout; got != domain.LayoutBlur {
		t.Fatalf("layout = %q, want blur", got)
	}
}
