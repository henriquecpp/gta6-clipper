package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/contracttest"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

type publishEnv struct {
	repo      *fakes.ClipRepo
	notifier  *fakes.Notifier
	publisher *fakes.Publisher
	metrics   *fakes.Metrics
	errs      *app.ErrorLog
	queued    []domain.ClipID
	accept    bool
	request   *app.RequestPublish
	publish   *app.PublishClip
}

func newPublishEnv(t *testing.T) *publishEnv {
	t.Helper()
	e := &publishEnv{repo: fakes.NewClipRepo(), notifier: &fakes.Notifier{}, publisher: &fakes.Publisher{},
		metrics: fakes.NewMetrics(), errs: &app.ErrorLog{}, accept: true}
	clock := fakes.NewClock(t0)
	e.request = app.NewRequestPublish(e.repo, e.notifier, clock, discardLog(), e.metrics, func(_ context.Context, id domain.ClipID) bool {
		e.queued = append(e.queued, id)
		return e.accept
	})
	e.publish = app.NewPublishClip(app.PublishConfig{
		Upload: domain.UploadOptions{
			TitleHashtags: []string{"#GTA6", "#Shorts"}, Hashtags: []string{"#GTA6", "#Shorts"},
			Tags: []string{"GTA 6", "GTA VI"}, TextLanguage: "pt-BR",
		},
		Streamers: map[string]domain.Streamer{"streamer_a": {Login: "streamer_a", Language: "en"}},
	}, e.repo, e.publisher, e.notifier, clock, discardLog(), e.metrics, e.errs)
	return e
}

// delivered stores a delivered clip with its final video.
func (e *publishEnv) delivered(t *testing.T, id domain.ClipID) {
	t.Helper()
	c := contracttest.FullClip(id, t0)
	for _, s := range []domain.ClipState{domain.StatePreviewSent, domain.StateApproved, domain.StateEditing, domain.StateRendered, domain.StateDelivered} {
		if err := c.Transition(s, t0); err != nil {
			t.Fatal(err)
		}
	}
	c.FinalMedia, c.FinalMessage = "/output/"+domain.MediaRef(id)+".mp4", "111:9"
	c.Headline, c.PostText = "Ele pulou da ponte!", "Momento insano na live de streamer_a #gta6"
	if err := e.repo.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func (e *publishEnv) get(t *testing.T, id domain.ClipID) *domain.Clip {
	t.Helper()
	c, err := e.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPublishHappyPath(t *testing.T) {
	e := newPublishEnv(t)
	ctx := context.Background()
	e.delivered(t, "c1")
	if err := e.request.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "c1"); got.State != domain.StatePublishing || !slices.Equal(e.queued, []domain.ClipID{"c1"}) {
		t.Fatalf("after request: %s, queued %v", got.State, e.queued)
	}
	// A second tap while the upload runs is rejected: never two uploads.
	if err := e.request.Execute(ctx, "c1"); !errors.Is(err, domain.ErrInvalidTransition) || len(e.queued) != 1 {
		t.Fatalf("duplicate request: %v (queued %v)", err, e.queued)
	}
	if err := e.publish.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	got := e.get(t, "c1")
	if got.State != domain.StatePublished || got.PublishedID != "yt001" || got.PublishedURL != "https://www.youtube.com/shorts/yt001" {
		t.Fatalf("published clip = %s %q %q", got.State, got.PublishedID, got.PublishedURL)
	}
	meta := e.publisher.Uploads[0]
	if meta.Title != "Ele pulou da ponte! #GTA6 #Shorts" || meta.AudioLanguage != "en" || meta.TextLanguage != "pt-BR" ||
		!strings.Contains(meta.Description, "twitch.tv/streamer_a") || !slices.Contains(meta.Tags, "streamer_a") ||
		e.publisher.Videos[0] != "/output/c1.mp4" {
		t.Fatalf("upload = %+v (video %s)", meta, e.publisher.Videos[0])
	}
	if !slices.Equal(e.notifier.FinalStates(), []domain.ClipState{domain.StatePublishing, domain.StatePublished}) {
		t.Fatalf("final message states = %v", e.notifier.FinalStates())
	}
	if e.metrics.StateCount(domain.StatePublished) != 1 || e.metrics.Stages["publish"] != 1 {
		t.Fatalf("metrics = %+v", e.metrics)
	}
	// Duplicate job and publishing again are no-ops / rejected.
	if err := e.publish.Execute(ctx, "c1"); err != nil || e.publisher.Count() != 1 {
		t.Fatalf("duplicate job: %v (uploads %d)", err, e.publisher.Count())
	}
	if err := e.request.Execute(ctx, "c1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("publish a published clip: %v", err)
	}
}

func TestPublishFailureGivesTheButtonBack(t *testing.T) {
	e := newPublishEnv(t)
	ctx := context.Background()
	e.delivered(t, "c1")
	e.publisher.Err = errors.New("youtube: 403 quotaExceeded")
	if err := e.request.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := e.publish.Execute(ctx, "c1"); err == nil {
		t.Fatal("expected error")
	}
	got := e.get(t, "c1")
	if got.State != domain.StateDelivered || !strings.Contains(got.PublishError, "quotaExceeded") {
		t.Fatalf("after failure: %s %q", got.State, got.PublishError)
	}
	if msg, _ := e.errs.Last(); !strings.Contains(msg, "quotaExceeded") {
		t.Fatalf("last error = %q", msg)
	}
	// The human taps again and it works.
	e.publisher.Err = nil
	if err := e.request.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := e.publish.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "c1"); got.State != domain.StatePublished || got.PublishError != "" {
		t.Fatalf("after retry: %s %q", got.State, got.PublishError)
	}
}

func TestPublishRequestRejections(t *testing.T) {
	e := newPublishEnv(t)
	ctx := context.Background()
	// Only delivered clips can be published.
	c := contracttest.FullClip("c-pending", t0)
	if err := e.repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := e.request.Execute(ctx, "c-pending"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("publish a detected clip: %v", err)
	}
	if err := e.request.Execute(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown clip: %v", err)
	}
	// Queue full: the clip goes back to delivered instead of hanging in publishing.
	e.delivered(t, "c1")
	e.accept = false
	if err := e.request.Execute(ctx, "c1"); err == nil {
		t.Fatal("expected error when the queue rejects the job")
	}
	if got := e.get(t, "c1"); got.State != domain.StateDelivered || got.PublishError == "" {
		t.Fatalf("after full queue: %s %q", got.State, got.PublishError)
	}
	// A clip without a final video cannot be uploaded.
	e.accept = true
	e.delivered(t, "c2")
	c2 := e.get(t, "c2")
	c2.FinalMedia = ""
	if err := e.repo.Save(ctx, c2); err != nil {
		t.Fatal(err)
	}
	_ = e.request.Execute(ctx, "c2")
	if err := e.publish.Execute(ctx, "c2"); err == nil || e.publisher.Count() != 0 {
		t.Fatalf("publish without video: %v (uploads %d)", err, e.publisher.Count())
	}
}

func TestInterruptedPublicationIsNotReuploaded(t *testing.T) {
	e := newPublishEnv(t)
	ctx := context.Background()
	e.delivered(t, "c1")
	if err := e.request.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	// Restart: the clip is still in publishing.
	var submitted []domain.ClipID
	r := app.NewRecoverPending(e.repo,
		func(context.Context, domain.ClipID) error { return nil },
		func(_ context.Context, id domain.ClipID) bool { submitted = append(submitted, id); return true },
		discardLog(), app.WithInterruptedPublications(e.publish.Interrupted))
	n, err := r.Execute(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recover = %d, %v", n, err)
	}
	got := e.get(t, "c1")
	if got.State != domain.StateDelivered || !strings.Contains(got.PublishError, "confira o canal") || e.publisher.Count() != 0 || len(submitted) != 0 {
		t.Fatalf("after recovery: %s %q (uploads %d, submitted %v)", got.State, got.PublishError, e.publisher.Count(), submitted)
	}
	if states := e.notifier.FinalStates(); states[len(states)-1] != domain.StateDelivered {
		t.Fatalf("final message states = %v", states)
	}
	// Interrupted on a clip that is not publishing is an error, not a change.
	if err := e.publish.Interrupted(ctx, "c1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("interrupt a delivered clip: %v", err)
	}
}

func TestPublishCancelledDuringUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newPublishEnv(t)
		e.delivered(t, "c1")
		if err := e.request.Execute(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
		e.publisher.Block = make(chan struct{}) // never released: the upload hangs
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.publish.Execute(ctx, "c1") }()
		synctest.Wait() // blocked inside Publish
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled upload: %v", err)
		}
		// The cancelled context cannot save either: the clip stays in
		// publishing and the next start hands it to Interrupted.
		if got := e.get(t, "c1"); got.State != domain.StatePublishing || e.publisher.Count() != 0 {
			t.Fatalf("after cancel: %s (uploads %d)", got.State, e.publisher.Count())
		}
	})
}
