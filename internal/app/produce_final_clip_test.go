package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/contracttest"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

type produceEnv struct {
	repo     *fakes.ClipRepo
	media    *fakes.MediaCapture
	trans    *fakes.Transcriber
	editor   *fakes.Editor
	renderer *fakes.Renderer
	notifier *fakes.Notifier
	clock    *fakes.Clock
	metrics  *fakes.Metrics
	produce  *app.ProduceFinalClip
}

func goodProposal() domain.EditProposal {
	return domain.EditProposal{
		StartS: 40, EndS: 70, Headline: "Ele pulou da ponte!",
		Captions: []domain.ProposedCaption{{StartS: 41, EndS: 44, Text: "Vocês viram isso?"}},
		PostText: "Momento insano na live de streamer_a", Reasoning: "contexto",
		ToolUseID: "toolu_1", RawInput: `{"start_s":40}`,
	}
}

var usage = domain.Usage{InputTokens: 6000, OutputTokens: 900, CacheWriteTokens: 2000}

// Haiku 5.5-like prices (µUSD per MTok); 6000 in + 900 out + 2000 cache write = 1300 µUSD.
var pricing = domain.Pricing{Input: 100_000, Output: 500_000, CacheWrite: 125_000, CacheRead: 10_000}

func newProduceEnv(t *testing.T) *produceEnv {
	t.Helper()
	e := &produceEnv{repo: fakes.NewClipRepo(), media: fakes.NewMediaCapture(), notifier: &fakes.Notifier{},
		clock: fakes.NewClock(t0), metrics: fakes.NewMetrics(), renderer: &fakes.Renderer{},
		trans: &fakes.Transcriber{Transcript: domain.Transcript{Language: "en", Words: []domain.Word{
			{Text: "no", Start: 39800 * time.Millisecond, End: 40200 * time.Millisecond},
			{Text: "way", Start: 41 * time.Second, End: 41500 * time.Millisecond},
		}}},
		editor: &fakes.Editor{Answers: []fakes.EditorAnswer{{Proposal: goodProposal(), Usage: usage}}},
	}
	e.produce = app.NewProduceFinalClip(app.ProduceConfig{
		MinDuration: 15 * time.Second, MaxDuration: 60 * time.Second, PeakTolerance: 5 * time.Second,
		HeadlineMaxChars: 60, SnapMargin: 150 * time.Millisecond, Pricing: pricing, MaxCost: 50_000,
		Width: 1080, Height: 1920, FPS: 30, MaxBytes: 45 << 20,
		Streamers: map[string]domain.Streamer{"streamer_a": {Login: "streamer_a", Layout: domain.LayoutStack}},
	}, e.repo, e.trans, e.editor, e.renderer, e.notifier, e.media, e.clock, discardLog(), e.metrics, nil)
	return e
}

// approved stores an approved clip.
func (e *produceEnv) approved(t *testing.T, id domain.ClipID) *domain.Clip {
	t.Helper()
	c := contracttest.FullClip(id, t0)
	c.PreviewMedia = c.SourceMedia + ".preview.mp4"
	e.media.Add(c.SourceMedia)
	e.media.Add(c.PreviewMedia)
	_ = c.Transition(domain.StatePreviewSent, t0)
	_ = c.Transition(domain.StateApproved, t0)
	if err := e.repo.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *produceEnv) get(t *testing.T, id domain.ClipID) *domain.Clip {
	t.Helper()
	c, err := e.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProduceHappyPath(t *testing.T) {
	e := newProduceEnv(t)
	ctx := context.Background()
	c := e.approved(t, "c1")
	if err := e.produce.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	got := e.get(t, "c1")
	if got.State != domain.StateDelivered || got.FinalMedia != "/output/c1.mp4" || got.Headline != "Ele pulou da ponte!" {
		t.Fatalf("clip = %s %s %q", got.State, got.FinalMedia, got.Headline)
	}
	if got.Cost != 1300 || got.Usage != usage {
		t.Fatalf("cost = %v usage = %+v", got.Cost, got.Usage)
	}
	// Start snapped out of the word "no" (39.8–40.2) to 39.65.
	if got.Plan == nil || got.Plan.Start != 39650*time.Millisecond {
		t.Fatalf("plan = %+v", got.Plan)
	}
	spec := e.renderer.Finals[0]
	if spec.Layout != domain.LayoutStack || spec.Width != 1080 || spec.Start != got.Plan.Start || len(spec.Words) != 2 {
		t.Fatalf("render spec = %+v", spec)
	}
	req := e.editor.Requests[0]
	if req.Constraints.Peak != 60*time.Second || req.Constraints.WindowDuration != 90*time.Second || req.Language != "en" ||
		req.OutputLanguage != "pt-BR" || req.Signals[0].Pos != 0 || req.Retry != nil || len(req.ViewerClipTitles) != 2 {
		t.Fatalf("editor request = %+v", req)
	}
	if len(e.notifier.Finals) != 1 || e.notifier.Finals[0].Text != got.PostText {
		t.Fatalf("delivered = %+v", e.notifier.Finals)
	}
	if e.media.IsLive(c.SourceMedia) || e.media.IsLive(c.PreviewMedia) {
		t.Fatal("pending media must be freed after delivery")
	}
	if e.metrics.Cost != 1300 || e.metrics.StateCount(domain.StateDelivered) != 1 || len(e.notifier.Alerts) != 0 {
		t.Fatalf("metrics/alerts = %+v %v", e.metrics, e.notifier.Alerts)
	}
	// Running again is a no-op.
	if err := e.produce.Execute(ctx, "c1"); err != nil || len(e.editor.Requests) != 1 {
		t.Fatalf("second run: %v, editor calls %d", err, len(e.editor.Requests))
	}
}

func TestProduceRetriesInvalidAnswerOnce(t *testing.T) {
	e := newProduceEnv(t)
	bad := goodProposal()
	bad.Headline = ""
	e.editor.Answers = []fakes.EditorAnswer{{Proposal: bad, Usage: usage}, {Proposal: goodProposal(), Usage: usage}}
	e.approved(t, "c1")
	if err := e.produce.Execute(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if len(e.editor.Requests) != 2 {
		t.Fatalf("editor calls = %d", len(e.editor.Requests))
	}
	retry := e.editor.Requests[1].Retry
	if retry == nil || !strings.Contains(retry.Problem, "headline") || retry.Previous.ToolUseID != "toolu_1" {
		t.Fatalf("retry = %+v", retry)
	}
	if got := e.get(t, "c1"); got.State != domain.StateDelivered || got.Usage.InputTokens != 12000 {
		t.Fatalf("clip = %s usage %+v (both calls must be billed)", got.State, got.Usage)
	}
}

func TestProduceFailsAfterSecondInvalidAnswer(t *testing.T) {
	e := newProduceEnv(t)
	e.editor.Answers = []fakes.EditorAnswer{{Err: errors.Join(domain.ErrInvalidEditPlan, errors.New("no tool_use")), Usage: usage}}
	e.approved(t, "c1")
	if err := e.produce.Execute(context.Background(), "c1"); !domain.IsInvalidEditPlan(err) {
		t.Fatalf("err = %v", err)
	}
	got := e.get(t, "c1")
	if got.State != domain.StateFailed || got.FailedFrom != domain.StateEditing || got.Usage.InputTokens != 12000 || got.Cost != 2600 {
		t.Fatalf("clip = %s from %s usage %+v cost %v", got.State, got.FailedFrom, got.Usage, got.Cost)
	}
	if len(e.editor.Requests) != 2 || len(e.notifier.Failures) != 1 {
		t.Fatalf("editor calls %d failures %v", len(e.editor.Requests), e.notifier.Failures)
	}
}

func TestProduceFailures(t *testing.T) {
	cases := map[string]func(e *produceEnv){
		"claude down":      func(e *produceEnv) { e.editor.Answers = []fakes.EditorAnswer{{Err: errors.New("529 overloaded")}} },
		"whisper down":     func(e *produceEnv) { e.trans.Err = errors.New("whisper server unavailable") },
		"render failed":    func(e *produceEnv) { e.renderer.FinalErr = errors.New("nvenc") },
		"telegram timeout": func(e *produceEnv) { e.notifier.FinalErr = context.DeadlineExceeded },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newProduceEnv(t)
			setup(e)
			e.approved(t, "c1")
			if err := e.produce.Execute(context.Background(), "c1"); err == nil {
				t.Fatal("expected error")
			}
			got := e.get(t, "c1")
			if got.State != domain.StateFailed || got.FailureReason == "" {
				t.Fatalf("clip = %s %q", got.State, got.FailureReason)
			}
		})
	}
	// Claude errors after a whisper failure must not have been called at all.
	e := newProduceEnv(t)
	e.trans.Err = errors.New("down")
	e.approved(t, "c2")
	_ = e.produce.Execute(context.Background(), "c2")
	if len(e.editor.Requests) != 0 {
		t.Fatal("editor called despite transcription failure")
	}
}

func TestProduceCostAlert(t *testing.T) {
	e := newProduceEnv(t)
	e.editor.Answers = []fakes.EditorAnswer{{Proposal: goodProposal(), Usage: domain.Usage{InputTokens: 500_000, OutputTokens: 20_000}}}
	e.approved(t, "c1")
	if err := e.produce.Execute(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if len(e.notifier.Alerts) != 1 || !strings.Contains(e.notifier.Alerts[0], "$0.060000 > $0.050000") {
		t.Fatalf("alerts = %v", e.notifier.Alerts)
	}
}

func TestRetryReusesPlanAndResumesFromRendered(t *testing.T) {
	ctx := context.Background()
	e := newProduceEnv(t)
	e.renderer.FinalErr = errors.New("disk full")
	e.approved(t, "c1")
	_ = e.produce.Execute(ctx, "c1")
	if got := e.get(t, "c1"); got.State != domain.StateFailed || got.Plan == nil {
		t.Fatalf("clip = %s plan %v", got.State, got.Plan)
	}
	var submitted []domain.ClipID
	retry := app.NewRetryClip(e.repo, e.notifier, e.clock, discardLog(), e.metrics, func(_ context.Context, id domain.ClipID) bool {
		submitted = append(submitted, id)
		return true
	})
	if err := retry.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := retry.Execute(ctx, "c1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("retry of an approved clip err = %v", err)
	}
	e.renderer.FinalErr = nil
	if err := e.produce.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if len(e.editor.Requests) != 1 || len(submitted) != 1 {
		t.Fatalf("editor calls %d (plan must be reused), submitted %v", len(e.editor.Requests), submitted)
	}
	if got := e.get(t, "c1"); got.State != domain.StateDelivered || got.Cost != 1300 {
		t.Fatalf("clip = %s cost %v", got.State, got.Cost)
	}

	// Rendered but not delivered (crash before Telegram): only delivery runs.
	c := e.approved(t, "c2")
	c, _ = e.repo.Get(ctx, c.ID)
	_ = c.Transition(domain.StateEditing, t0)
	c.FinalMedia, c.Headline, c.PostText = "/output/c2.mp4", "h", "p streamer_a"
	_ = c.Transition(domain.StateRendered, t0)
	_ = e.repo.Save(ctx, c)
	calls := e.trans.Calls
	if err := e.produce.Execute(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	if e.trans.Calls != calls || e.get(t, "c2").State != domain.StateDelivered {
		t.Fatalf("transcriber calls %d", e.trans.Calls)
	}
}

func TestRecoverPending(t *testing.T) {
	ctx := context.Background()
	repo := fakes.NewClipRepo()
	save := func(id domain.ClipID, states ...domain.ClipState) {
		c := contracttest.FullClip(id, t0)
		for _, s := range states {
			if err := c.Transition(s, t0); err != nil {
				t.Fatal(err)
			}
		}
		_ = repo.Save(ctx, c)
	}
	save("d1")
	save("p1", domain.StatePreviewSent)
	save("a1", domain.StatePreviewSent, domain.StateApproved)
	save("e1", domain.StatePreviewSent, domain.StateApproved, domain.StateEditing)
	save("r1", domain.StatePreviewSent, domain.StateApproved, domain.StateEditing, domain.StateRendered)
	save("x1", domain.StatePreviewSent, domain.StateDiscarded)
	var previews, submitted []domain.ClipID
	r := app.NewRecoverPending(repo,
		func(_ context.Context, id domain.ClipID) error { previews = append(previews, id); return nil },
		func(_ context.Context, id domain.ClipID) bool { submitted = append(submitted, id); return true },
		discardLog())
	n, err := r.Execute(ctx)
	if err != nil || n != 4 {
		t.Fatalf("resumed = %d, %v", n, err)
	}
	if len(previews) != 1 || previews[0] != "d1" || len(submitted) != 3 {
		t.Fatalf("previews %v submitted %v", previews, submitted)
	}
	failing := app.NewRecoverPending(repo, func(context.Context, domain.ClipID) error { return errors.New("x") },
		func(context.Context, domain.ClipID) bool { return false }, discardLog())
	if _, err := failing.Execute(ctx); err == nil {
		t.Fatal("expected error")
	}
}

func TestProduceQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var done []domain.ClipID
		release := make(chan struct{})
		produce := func(ctx context.Context, id domain.ClipID) error {
			if id == "slow" {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			mu.Lock()
			done = append(done, id)
			mu.Unlock()
			return nil
		}
		m := fakes.NewMetrics()
		q := app.NewJobQueue("produce", produce, 1, 2, 10*time.Second, discardLog(), m)
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		go func() { finished <- q.Run(ctx) }()

		if !q.Submit(ctx, "a") || q.Submit(ctx, "a") {
			t.Fatal("dedup")
		}
		synctest.Wait()
		if !q.Submit(ctx, "slow") {
			t.Fatal("submit slow")
		}
		synctest.Wait() // slow is running
		if !q.Submit(ctx, "b") || !q.Submit(ctx, "c") || q.Submit(ctx, "d") {
			t.Fatal("capacity 2 must reject the third waiting job")
		}
		if q.Depth() != 3 {
			t.Fatalf("depth = %d", q.Depth())
		}
		// Shutdown while "slow" runs: it gets the grace period, b and c stay queued.
		cancel()
		synctest.Wait()
		if q.Submit(context.Background(), "e") {
			t.Fatal("closed queue accepted a job")
		}
		close(release)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(done) != 2 || done[0] != "a" || done[1] != "slow" {
			t.Fatalf("done = %v", done)
		}
	})
}

func TestProduceQueueGraceExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan struct{})
		q := app.NewJobQueue("produce", func(ctx context.Context, _ domain.ClipID) error {
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		}, 1, 1, 5*time.Second, discardLog(), nil)
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		go func() { finished <- q.Run(ctx) }()
		q.Submit(ctx, "stuck")
		synctest.Wait()
		cancel()
		time.Sleep(6 * time.Second)
		<-cancelled
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	})
}

func TestProduceTranscribesInTheStreamerLanguage(t *testing.T) {
	ctx := context.Background()
	game := domain.Transcript{Language: "en", Words: []domain.Word{{Text: "Love", Start: time.Second, End: 2 * time.Second}}}
	for _, tc := range []struct {
		name      string
		lang      string
		gameErr   error
		wantLangs []string
		wantGame  bool
	}{
		{name: "pt streamer gets the game-language pass", lang: "pt", wantLangs: []string{"pt", "en"}, wantGame: true},
		{name: "en streamer needs no second pass", lang: "en", wantLangs: []string{"en"}},
		{name: "second pass failure is not fatal", lang: "pt", gameErr: errors.New("whisper busy"), wantLangs: []string{"pt", "en"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newProduceEnv(t)
			e.produce = app.NewProduceFinalClip(app.ProduceConfig{
				MinDuration: 15 * time.Second, MaxDuration: 60 * time.Second, PeakTolerance: 5 * time.Second,
				HeadlineMaxChars: 60, SnapMargin: 150 * time.Millisecond, Pricing: pricing, MaxCost: 50_000,
				Width: 1080, Height: 1920, FPS: 30, MaxBytes: 45 << 20, GameLanguage: "en",
				Streamers: map[string]domain.Streamer{"streamer_a": {Login: "streamer_a", Language: tc.lang}},
			}, e.repo, e.trans, e.editor, e.renderer, e.notifier, e.media, e.clock, discardLog(), e.metrics, nil)
			e.trans.ByLanguage = map[string]fakes.TranscriptAnswer{"en": {Transcript: game, Err: tc.gameErr}}
			if tc.lang == "en" {
				e.trans.ByLanguage = nil
			}
			e.approved(t, "c1")
			if err := e.produce.Execute(ctx, "c1"); err != nil {
				t.Fatal(err)
			}
			if strings.Join(e.trans.Languages, ",") != strings.Join(tc.wantLangs, ",") {
				t.Fatalf("languages = %v, want %v", e.trans.Languages, tc.wantLangs)
			}
			req := e.editor.Requests[0]
			if got := len(req.GameTranscript.Words) > 0; got != tc.wantGame {
				t.Fatalf("game transcript sent = %v, want %v", got, tc.wantGame)
			}
			if e.get(t, "c1").State != domain.StateDelivered {
				t.Fatal("clip not delivered")
			}
		})
	}
}
