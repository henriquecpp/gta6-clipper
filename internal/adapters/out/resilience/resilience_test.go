package resilience

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

type counter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *counter) ExternalError(port string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[port]++
}

type tempErr struct{ temp bool }

func (e tempErr) Error() string   { return fmt.Sprintf("temp=%v", e.temp) }
func (e tempErr) Temporary() bool { return e.temp }

func testDeps(m *counter, slept *[]time.Duration) Deps {
	return Deps{
		Log: slog.New(slog.DiscardHandler), Metrics: m,
		rand: func() float64 { return 0.5 },
		sleep: func(ctx context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return ctx.Err()
		},
	}.withDefaults()
}

func TestRetryable(t *testing.T) {
	cases := map[error]bool{
		nil:              false,
		context.Canceled: false,
		fmt.Errorf("x: %w", context.DeadlineExceeded):  false,
		fmt.Errorf("x: %w", domain.ErrInvalidEditPlan): false,
		domain.ErrNotFound:                             false,
		domain.ErrWindowUnavailable:                    false,
		tempErr{temp: false}:                           false,
		fmt.Errorf("wrapped: %w", tempErr{temp: true}): true,
		errors.New("connection reset"):                 true,
	}
	for err, want := range cases {
		if got := Retryable(err); got != want {
			t.Errorf("Retryable(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestPolicyDelay(t *testing.T) {
	p := Policy{Base: time.Second, Max: 5 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		if got := p.Delay(i+1, nil); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
	j := Policy{Base: time.Second, Max: time.Second, Jitter: 0.2}
	if d := j.Delay(1, func() float64 { return 1 }); d != 1200*time.Millisecond {
		t.Fatalf("jitter delay = %v", d)
	}
}

func TestDecoratorRetriesThenSucceeds(t *testing.T) {
	m := &counter{}
	var slept []time.Duration
	p := &fakes.Platform{}
	p.SetStreams(domain.LiveStream{ID: "1", Login: "a"})
	calls := 0
	flaky := &flakyPlatform{Platform: p, failures: 2, calls: &calls}
	d := NewPlatform(flaky, Policy{Attempts: 3, Base: time.Second, Max: 10 * time.Second}, testDeps(m, &slept))
	got, err := d.LiveStreams(context.Background(), []string{"a"})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if calls != 3 || m.n["platform"] != 2 || len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Fatalf("calls=%d errors=%v slept=%v", calls, m.n, slept)
	}
}

type flakyPlatform struct {
	*fakes.Platform
	failures int
	calls    *int
}

func (f *flakyPlatform) LiveStreams(ctx context.Context, logins []string) ([]domain.LiveStream, error) {
	*f.calls++
	if *f.calls <= f.failures {
		return nil, errors.New("connection reset by peer")
	}
	return f.Platform.LiveStreams(ctx, logins)
}

func TestDecoratorGivesUpAndSkipsPermanentErrors(t *testing.T) {
	m := &counter{}
	var slept []time.Duration
	deps := testDeps(m, &slept)

	tr := &fakes.Transcriber{Err: errors.New("whisper server unavailable")}
	dt := NewTranscriber(tr, Policy{Attempts: 3, Base: time.Second, Max: time.Second}, deps)
	if _, err := dt.Transcribe(context.Background(), "x", "pt"); err == nil || tr.Calls != 3 {
		t.Fatalf("err = %v calls = %d", err, tr.Calls)
	}

	ed := &fakes.Editor{Answers: []fakes.EditorAnswer{{Err: fmt.Errorf("%w: no tool call", domain.ErrInvalidEditPlan), Usage: domain.Usage{InputTokens: 10}}}}
	de := NewEditor(ed, EditorPolicy, deps)
	_, usage, err := de.Plan(context.Background(), domain.EditRequest{})
	if !errors.Is(err, domain.ErrInvalidEditPlan) || len(ed.Requests) != 1 || usage.InputTokens != 10 {
		t.Fatalf("invalid plan must not be retried: err=%v calls=%d usage=%+v", err, len(ed.Requests), usage)
	}
	ed2 := &fakes.Editor{Answers: []fakes.EditorAnswer{{Err: tempErr{temp: true}}, {Proposal: domain.EditProposal{Headline: "ok"}, Usage: domain.Usage{InputTokens: 5}}}}
	p, usage, err := NewEditor(ed2, EditorPolicy, deps).Plan(context.Background(), domain.EditRequest{})
	if err != nil || p.Headline != "ok" || usage.InputTokens != 5 || len(ed2.Requests) != 2 {
		t.Fatalf("temporary API error must be retried: %v %+v %d", err, usage, len(ed2.Requests))
	}

	mc := fakes.NewMediaCapture()
	mc.RetainErr = domain.ErrWindowUnavailable
	dm := NewMediaCapture(mc, FFmpegPolicy, deps)
	if _, _, err := dm.RetainWindow(context.Background(), "s", 0, 1); !errors.Is(err, domain.ErrWindowUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if m.n["editor"] != 2 || m.n["capture"] != 1 || m.n["transcriber"] != 3 {
		t.Fatalf("error counts = %v", m.n)
	}

	// Cancellation stops the retries.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr2 := &fakes.Transcriber{Err: errors.New("down")}
	if _, err := NewTranscriber(tr2, WhisperPolicy, deps).Transcribe(ctx, "x", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestAllDecoratorsDelegate(t *testing.T) {
	ctx := context.Background()
	deps := Deps{Log: slog.New(slog.DiscardHandler)}
	media := fakes.NewMediaCapture()
	dm := NewMediaCapture(media, FFmpegPolicy, deps)
	ref, w, err := dm.RetainWindow(ctx, "s1", 10, 20)
	if err != nil || ref == "" || w.To != 20 {
		t.Fatalf("retain = %q %v %v", ref, w, err)
	}
	if err := dm.Delete(ctx, ref); err != nil || media.IsLive(ref) {
		t.Fatal("delete")
	}
	rend := &fakes.Renderer{}
	dr := NewRenderer(rend, FFmpegPolicy, deps)
	if _, err := dr.Preview(ctx, "/p/a.ts", domain.PreviewSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dr.Final(ctx, "/p/a.ts", domain.RenderSpec{ClipID: "c"}); err != nil {
		t.Fatal(err)
	}
	n := &fakes.Notifier{}
	dn := NewNotifier(n, NetworkPolicy, deps)
	c := &domain.Clip{ID: "c"}
	if _, err := dn.SendPreview(ctx, c, "p"); err != nil {
		t.Fatal(err)
	}
	_ = dn.UpdatePreview(ctx, c)
	_ = dn.SendFailure(ctx, c, "x")
	if ref, err := dn.SendFinal(ctx, c, "v", domain.Post{}); err != nil || ref == "" {
		t.Fatalf("send final = %q %v", ref, err)
	}
	_ = dn.UpdateFinal(ctx, c)
	_ = dn.SendAlert(ctx, c, "y")
	if len(n.Sent) != 1 || len(n.Updates) != 1 || len(n.Failures) != 1 || len(n.Finals) != 1 || len(n.Alerts) != 1 || len(n.FinalUpdates) != 1 {
		t.Fatalf("notifier calls = %+v", n)
	}
	// Uploads are never retried: one failure is returned as is.
	pub := &fakes.Publisher{Err: errors.New("503 backend error")}
	if _, err := NewPublisher(pub, PublishPolicy, deps).Publish(ctx, "/output/v.mp4", domain.UploadMeta{}); err == nil {
		t.Fatal("publish error swallowed")
	}
	pub.Err = nil
	if p, err := NewPublisher(pub, PublishPolicy, deps).Publish(ctx, "/output/v.mp4", domain.UploadMeta{Title: "t"}); err != nil || p.ID == "" || pub.Count() != 1 {
		t.Fatalf("publish = %+v %v (uploads %d)", p, err, pub.Count())
	}
	pl := &fakes.Platform{Clips: map[string][]domain.ViewerClip{"b": {{ID: "x", CreatedAt: time.Now()}}}}
	if got, err := NewPlatform(pl, NetworkPolicy, deps).RecentClips(ctx, "b", time.Time{}); err != nil || len(got) != 1 {
		t.Fatalf("clips = %v %v", got, err)
	}
	// Real sleep honours cancellation.
	d := Deps{}.withDefaults()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.sleep(cctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("sleep must stop on cancel")
	}
	if err := d.sleep(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
