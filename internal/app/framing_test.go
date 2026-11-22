package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func TestFramingAuto(t *testing.T) {
	ctx := context.Background()
	cam := domain.Rect{X: 0.775, Y: 0.453, W: 0.217, H: 0.286}
	moved := domain.Rect{X: 0, Y: 0.35, W: 0.277, H: 0.272}
	configured := domain.StackRegions{Facecam: domain.Rect{X: 0.7, W: 0.3, H: 0.3}, Gameplay: domain.FullFrame}
	streamers := map[string]domain.Streamer{
		"auto_cfg": {Login: "auto_cfg", Layout: domain.LayoutAuto, Stack: configured},
		"auto":     {Login: "auto", Layout: domain.LayoutAuto},
		"blurry":   {Login: "blurry", Layout: domain.LayoutBlur},
	}
	det := &fakes.FacecamDetector{Answers: []fakes.FacecamAnswer{
		{Rect: cam, OK: true},            // found
		{},                               // not found: last position (fresh)
		{Err: errors.New("ffmpeg died")}, // error: last position too
		{Rect: moved, OK: true},          // the streamer moved the camera
		{},                               // later, past the TTL: configured / blur
	}}
	clock := fakes.NewClock(t0)
	f := app.NewFraming(det, streamers, clock, discardLog(), fakes.NewMetrics())
	clip := func(login string) *domain.Clip {
		return &domain.Clip{ID: "c", Streamer: login, SourceMedia: "/pending/x.ts"}
	}

	if l, s := f.Resolve(ctx, clip("auto_cfg")); l != domain.LayoutStack || s.Facecam != cam || s.Gameplay != configured.Gameplay {
		t.Fatalf("found: %s %+v", l, s)
	}
	if l, s := f.Resolve(ctx, clip("auto_cfg")); l != domain.LayoutStack || s.Facecam != cam {
		t.Fatalf("not found, last position: %s %+v", l, s)
	}
	if l, s := f.Resolve(ctx, clip("auto_cfg")); l != domain.LayoutStack || s.Facecam != cam {
		t.Fatalf("error, last position: %s %+v", l, s)
	}
	if _, s := f.Resolve(ctx, clip("auto_cfg")); s.Facecam != moved {
		t.Fatalf("moved camera not followed: %+v", s)
	}
	clock.Advance(2 * time.Hour)
	if l, s := f.Resolve(ctx, clip("auto_cfg")); l != domain.LayoutStack || s != configured {
		t.Fatalf("stale: want the configured regions, got %s %+v", l, s)
	}
	if l, _ := f.Resolve(ctx, clip("auto")); l != domain.LayoutBlur {
		t.Fatalf("auto without regions nor detection: %s", l)
	}
	calls := det.Calls
	if l, _ := f.Resolve(ctx, clip("blurry")); l != domain.LayoutBlur || det.Calls != calls {
		t.Fatalf("fixed layouts must not run the detector (%s, calls %d)", l, det.Calls)
	}
	// Without a detector, auto uses the fallbacks.
	if l, s := app.NewFraming(nil, streamers, clock, discardLog(), nil).Resolve(ctx, clip("auto_cfg")); l != domain.LayoutStack || s != configured {
		t.Fatalf("no detector: %s %+v", l, s)
	}
}

func TestPreviewAndFinalShareTheFraming(t *testing.T) {
	ctx := context.Background()
	cam := domain.Rect{X: 0, Y: 0.47, W: 0.255, H: 0.283}
	streamers := map[string]domain.Streamer{"streamer_a": {Login: "streamer_a", Layout: domain.LayoutAuto}}
	det := &fakes.FacecamDetector{Answers: []fakes.FacecamAnswer{{Rect: cam, OK: true}}}

	e := newPreviewEnv(t)
	framing := app.NewFraming(det, streamers, e.clock, discardLog(), nil)
	cfg := app.PreviewConfig{Duration: 30 * time.Second, Before: 10 * time.Second, Width: 480, Height: 854, FPS: 30, MaxBytes: 45 << 20, Streamers: streamers}
	send := app.NewSendPreview(cfg, e.repo, e.renderer, e.notifier, e.clock, discardLog(), nil, nil, app.WithFraming(framing))
	e.detected(t, "c1")
	if err := send.Execute(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	spec := e.renderer.Previews[0]
	got := e.get(t, "c1")
	if spec.Layout != domain.LayoutStack || spec.Stack.Facecam != cam || got.Layout != domain.LayoutStack || got.Stack.Facecam != cam {
		t.Fatalf("preview spec %s %+v, clip %s %+v", spec.Layout, spec.Stack, got.Layout, got.Stack)
	}

	// The final render reuses the clip's framing, without detecting again.
	p := newProduceEnv(t)
	c := p.approved(t, "c2")
	c.Layout, c.Stack = domain.LayoutStack, domain.StackRegions{Facecam: cam, Gameplay: domain.FullFrame}
	if err := p.repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := p.produce.Execute(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	if fs := p.renderer.Finals[0]; fs.Layout != domain.LayoutStack || fs.Stack.Facecam != cam {
		t.Fatalf("final spec %s %+v", fs.Layout, fs.Stack)
	}
	if det.Calls != 1 {
		t.Fatalf("detector called %d times", det.Calls)
	}
}
