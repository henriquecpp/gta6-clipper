package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/domain/detect"
)

var captureStart = time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)

func detectionConfig() app.DetectionConfig {
	return app.DetectionConfig{
		Tick: 500 * time.Millisecond,
		Scorer: detect.ScorerConfig{
			Weights:     map[domain.SignalKind]float64{domain.SignalVoice: 0.45, domain.SignalChat: 0.45, domain.SignalClips: 0.10},
			ThresholdOn: 3, ThresholdOff: 1.5, ZMax: 6, MinSignalZ: 2,
			Warmup: 60 * time.Second, Cooldown: 60 * time.Second,
		},
		Voice:           detect.VoiceConfig{Window: 400 * time.Millisecond, Alpha: 0.02, StaleAfter: 2 * time.Second},
		Chat:            detect.ChatConfig{Window: 10 * time.Second, Sustain: 3 * time.Second, Alpha: 0.05},
		TermsByLanguage: map[string]map[string]float64{"en": {"KEKW": 2}},
		Clips:           detect.ClipsConfig{GroupWindow: 15 * time.Second, MinClips: 3},
		PreRoll:         60 * time.Second,
		PostRoll:        30 * time.Second,
		BufferDuration:  10 * time.Minute,
		RetainMargin:    3 * time.Second,
	}
}

var testStreamer = domain.Streamer{
	Login: "streamer", Language: "en", AllowsClips: true, Layout: domain.LayoutBlur,
	Offsets: domain.Offsets{CaptureLatency: 4 * time.Second, Chat: 8 * time.Second, Clip: 5 * time.Second},
}

var testLive = domain.LiveStream{ID: "s1", BroadcasterID: "b1", Login: "streamer", Game: "GTA VI", Title: "t", Viewers: 1000, StartedAt: captureStart.Add(-time.Hour)}

type harness struct {
	t        *testing.T
	clock    *fakes.Clock
	repo     *fakes.ClipRepo
	capture  *fakes.MediaCapture
	metrics  *fakes.Metrics
	d        *app.DetectMoment
	framePos time.Duration
	mu       sync.Mutex
	handled  []*domain.Clip
	ids      int
	tickErrs []error
}

func newHarness(t *testing.T, withCapture bool) *harness {
	h := &harness{t: t, clock: fakes.NewClock(captureStart), repo: fakes.NewClipRepo(), capture: fakes.NewMediaCapture(), metrics: fakes.NewMetrics()}
	h.capture.Pad = domain.StreamPos(time.Second)
	opts := []app.DetectMomentOption{
		app.WithClipHandler(func(_ context.Context, c *domain.Clip) { h.mu.Lock(); h.handled = append(h.handled, c); h.mu.Unlock() }),
		app.WithDetectMetrics(h.metrics),
	}
	if withCapture {
		opts = append(opts, app.WithCapture(h.capture))
	}
	newID := func() (domain.ClipID, error) { h.ids++; return domain.ClipID(fmt.Sprintf("clip-%d", h.ids)), nil }
	h.d = app.NewDetectMoment(detectionConfig(), h.repo, newID, h.clock, discardLog(), opts...)
	h.d.StartStream(testStreamer, testLive, captureStart)
	return h
}

// level is the speech level of the streamer at a capture position.
func (h *harness) feedFrames(to time.Duration, level func(time.Duration) float64) {
	for ; h.framePos < to; h.framePos += 30 * time.Millisecond {
		db := level(h.framePos)
		jitter := []float64{-1.5, 0.5, 1.5, -0.5}[int(h.framePos/(30*time.Millisecond))%4]
		h.d.ObserveAudio(testLive.ID, domain.AudioFrame{Pos: domain.StreamPos(h.framePos), DBFS: db + jitter, Speech: true})
	}
}

// run advances the fake time in 100 ms steps until `until` (since capture
// start), feeding frames, calling chat(now) and ticking every 500 ms.
func (h *harness) run(from, until time.Duration, level func(time.Duration) float64, chat func(now time.Duration)) {
	for now := from; now <= until; now += 100 * time.Millisecond {
		h.clock.Set(captureStart.Add(now))
		h.feedFrames(now, level)
		if chat != nil {
			chat(now)
		}
		if now%(500*time.Millisecond) == 0 {
			if err := h.d.Tick(context.Background(), testLive.ID); err != nil {
				h.tickErrs = append(h.tickErrs, err)
			}
		}
	}
}

func steadyVoice(time.Duration) float64 { return -30 }

// shoutAt is a steady voice with 2 s shouts at the given positions: a voice
// reaction that alone stays below threshold_on (0.45 × z_max < 3).
func shoutAt(at ...time.Duration) func(time.Duration) float64 {
	return func(p time.Duration) float64 {
		for _, a := range at {
			if p >= a && p < a+2*time.Second {
				return -12
			}
		}
		return -30
	}
}

// steadyChat sends 2 messages per second.
func (h *harness) steadyChat(now time.Duration) {
	if now%(500*time.Millisecond) == 0 {
		h.d.ObserveChat(testLive.ID, captureStart.Add(now), "hi")
	}
}

func TestDetectMomentHappyPath(t *testing.T) {
	h := newHarness(t, true)
	event := 300 * time.Second // capture position of the moment
	voice := func(p time.Duration) float64 {
		if p >= event && p < event+2*time.Second {
			return -12 // shouting
		}
		return -30
	}
	chat := func(now time.Duration) {
		h.steadyChat(now)
		// Chat reacts 8 s after the event (= 4 s after the capture shows it):
		// a laughing burst received between event+4s and event+12s.
		if now >= event+4*time.Second && now < event+12*time.Second {
			for range 2 {
				h.d.ObserveChat(testLive.ID, captureStart.Add(now), "KEKW KEKW")
			}
		}
	}
	h.run(0, 400*time.Second, voice, chat)

	if len(h.tickErrs) != 0 {
		t.Fatalf("tick errors: %v", h.tickErrs)
	}
	clips, _ := h.repo.ListByState(context.Background(), domain.StateDetected)
	if len(clips) != 1 {
		t.Fatalf("clips = %d, want 1", len(clips))
	}
	c := clips[0]
	peak := time.Duration(c.Moment.Peak)
	if peak < event || peak > event+10*time.Second {
		t.Fatalf("peak at %v, want near %v", peak, event)
	}
	if c.Moment.Values[domain.SignalVoice] < 2 || c.Moment.Values[domain.SignalChat] < 2 {
		t.Fatalf("signals at peak = %v", c.Moment.Values)
	}
	// The capture retains whole segments: the clip records the retained window.
	if c.Window.From != c.Moment.Peak-domain.StreamPos(61*time.Second) || c.Window.To != c.Moment.Peak+domain.StreamPos(31*time.Second) {
		t.Fatalf("window = %+v for peak %v", c.Window, c.Moment.Peak)
	}
	if len(h.capture.Calls) != 1 || h.capture.Calls[0].From != c.Moment.Peak-domain.StreamPos(60*time.Second) || h.capture.Calls[0].StreamID != "s1" {
		t.Fatalf("retain calls = %+v", h.capture.Calls)
	}
	if c.StreamID != "s1" || c.Game != "GTA VI" || c.Streamer != "streamer" || !h.capture.IsLive(c.SourceMedia) {
		t.Fatalf("clip = %+v", c)
	}
	if n := len(c.Timeline); n < 92 || n > 93 {
		t.Fatalf("timeline has %d points, want one per second over the 92 s window", n)
	}
	if len(h.handled) != 1 || h.handled[0].ID != c.ID {
		t.Fatalf("clip handler calls = %d", len(h.handled))
	}
	if h.metrics.Moments["streamer"] != 1 || h.metrics.StateCount(domain.StateDetected) != 1 || h.metrics.Stages["retain"] != 1 {
		t.Fatalf("metrics = %+v", h.metrics)
	}
}

func TestDetectMomentWithoutCaptureOnlyLogs(t *testing.T) {
	h := newHarness(t, false)
	voice := func(p time.Duration) float64 {
		if p >= 200*time.Second && p < 202*time.Second {
			return -10
		}
		return -30
	}
	chat := func(now time.Duration) {
		h.steadyChat(now)
		if now >= 204*time.Second && now < 212*time.Second {
			for range 3 {
				h.d.ObserveChat(testLive.ID, captureStart.Add(now), "KEKW")
			}
		}
	}
	h.run(0, 280*time.Second, voice, chat)
	if h.metrics.Moments["streamer"] != 1 {
		t.Fatalf("moments = %d, want 1", h.metrics.Moments["streamer"])
	}
	if h.repo.Len() != 0 || len(h.handled) != 0 {
		t.Fatal("nothing should be persisted without capture")
	}
}

func lateClips(center time.Duration, n int, prefix string) []domain.ViewerClip {
	// Created at the wall time that maps to `center`: pos = created − clip − dur/2 + latency − capStart.
	var out []domain.ViewerClip
	for i := range n {
		created := captureStart.Add(center + 5*time.Second + 15*time.Second - 4*time.Second + time.Duration(i)*2*time.Second)
		out = append(out, domain.ViewerClip{ID: fmt.Sprintf("%s-%d", prefix, i), CreatedAt: created, Duration: 30 * time.Second})
	}
	return out
}

func TestDetectMomentLateClipMoment(t *testing.T) {
	h := newHarness(t, true)
	h.run(0, 200*time.Second, shoutAt(150*time.Second), h.steadyChat)
	if h.repo.Len() != 0 {
		t.Fatal("a shout alone must not trigger a fast moment")
	}
	// Three viewer clips around 150 s arrive late (at 200 s).
	if err := h.d.ObserveClips(context.Background(), testLive.ID, lateClips(150*time.Second, 3, "v")); err != nil {
		t.Fatal(err)
	}
	h.run(200*time.Second, 210*time.Second, steadyVoice, h.steadyChat)
	clips, _ := h.repo.ListByState(context.Background(), domain.StateDetected)
	if len(clips) != 1 {
		t.Fatalf("clips = %d, want 1 late clip", len(clips))
	}
	c := clips[0]
	if !c.Moment.Late || c.Moment.ViewerClips != 3 || c.Moment.Values[domain.SignalClips] != 3 {
		t.Fatalf("late moment = %+v", c.Moment)
	}
	if d := time.Duration(c.Moment.Peak) - 150*time.Second; d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("late peak = %v", c.Moment.Peak)
	}
	// Re-sending the same clips changes nothing.
	if err := h.d.ObserveClips(context.Background(), testLive.ID, lateClips(150*time.Second, 3, "v")); err != nil {
		t.Fatal(err)
	}
	h.run(210*time.Second, 260*time.Second, steadyVoice, h.steadyChat)
	if h.repo.Len() != 1 {
		t.Fatalf("duplicate clips created a new moment: %d", h.repo.Len())
	}
	// Clips far outside the buffer are ignored.
	if err := h.d.ObserveClips(context.Background(), testLive.ID, lateClips(-20*time.Minute, 3, "old")); err != nil {
		t.Fatal(err)
	}
}

func TestDetectMomentIgnoresClipsBeforeCaptureStart(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(t, true)
	h.d = app.NewDetectMoment(detectionConfig(), h.repo, func() (domain.ClipID, error) { return "c", nil }, h.clock,
		slog.New(slog.NewTextHandler(&logs, nil)), app.WithCapture(h.capture), app.WithDetectMetrics(h.metrics))
	h.d.StartStream(testStreamer, testLive, captureStart)
	h.run(0, 40*time.Second, steadyVoice, h.steadyChat)
	// Viewers clipped something 30 s before the capture started, and more
	// clips of it keep arriving with every poll.
	for i := range 3 {
		from := 40*time.Second + time.Duration(i)*30*time.Second
		if err := h.d.ObserveClips(context.Background(), testLive.ID, lateClips(-30*time.Second, 3+i, fmt.Sprintf("pre%d", i))); err != nil {
			t.Fatal(err)
		}
		h.run(from, from+30*time.Second, steadyVoice, h.steadyChat)
	}
	if h.repo.Len() != 0 || len(h.capture.Calls) != 0 {
		t.Fatalf("clips = %d, retain calls = %d; want none", h.repo.Len(), len(h.capture.Calls))
	}
	if s := logs.String(); strings.Contains(s, "late moment") || strings.Contains(s, "moment dropped") {
		t.Fatalf("pre-capture clips were proposed as moments:\n%s", s)
	}
}

func TestDetectMomentLateClipsNeedAReaction(t *testing.T) {
	h := newHarness(t, true)
	h.run(0, 200*time.Second, steadyVoice, h.steadyChat)
	// Viewers clipped 150 s, but neither the streamer nor the chat reacted.
	if err := h.d.ObserveClips(context.Background(), testLive.ID, lateClips(150*time.Second, 5, "quiet")); err != nil {
		t.Fatal(err)
	}
	h.run(200*time.Second, 240*time.Second, steadyVoice, h.steadyChat)
	if h.repo.Len() != 0 {
		t.Fatalf("clips = %d; a cluster without voice or chat reaction must not become a moment", h.repo.Len())
	}
}

func TestDetectMomentLateClipsRespectCooldown(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	h.run(0, 300*time.Second, shoutAt(150*time.Second, 182*time.Second, 260*time.Second), h.steadyChat)
	step := func(from time.Duration, clips []domain.ViewerClip) {
		t.Helper()
		if err := h.d.ObserveClips(ctx, testLive.ID, clips); err != nil {
			t.Fatal(err)
		}
		h.run(from, from+5*time.Second, steadyVoice, h.steadyChat)
	}
	step(300*time.Second, lateClips(150*time.Second, 3, "a")) // accepted
	step(305*time.Second, lateClips(260*time.Second, 3, "b")) // accepted, now the latest peak
	// Arrives last but sits 32 s after the first moment (beyond the ±15 s
	// group window, inside the 60 s cooldown): no new clip.
	step(310*time.Second, lateClips(182*time.Second, 3, "c"))
	clips, _ := h.repo.ListByState(ctx, domain.StateDetected)
	if len(clips) != 2 {
		t.Fatalf("clips = %d, want 2 (the 182 s cluster is inside the cooldown of 150 s)", len(clips))
	}
}

func TestDetectMomentClipsBoostAcceptedClip(t *testing.T) {
	h := newHarness(t, true)
	h.run(0, 200*time.Second, shoutAt(150*time.Second), h.steadyChat)
	_ = h.d.ObserveClips(context.Background(), testLive.ID, lateClips(150*time.Second, 3, "a"))
	h.run(200*time.Second, 205*time.Second, steadyVoice, h.steadyChat)
	clips, _ := h.repo.ListByState(context.Background(), domain.StateDetected)
	if len(clips) != 1 {
		t.Fatalf("clips = %d", len(clips))
	}
	before := clips[0].Moment.Score
	// Two more clips of the same moment: the persisted clip is boosted.
	if err := h.d.ObserveClips(context.Background(), testLive.ID, lateClips(152*time.Second, 2, "b")); err != nil {
		t.Fatal(err)
	}
	got, _ := h.repo.Get(context.Background(), clips[0].ID)
	if got.Moment.ViewerClips != 5 || got.Moment.Values[domain.SignalClips] != 5 || got.Moment.Score <= before {
		t.Fatalf("boost not applied: %+v (score before %v)", got.Moment, before)
	}
}

func TestDetectMomentFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk full")

	// Retain failure: error returned, nothing persisted.
	h := newHarness(t, true)
	h.capture.RetainErr = boom
	h.run(0, 200*time.Second, shoutAt(150*time.Second), h.steadyChat)
	_ = h.d.ObserveClips(ctx, testLive.ID, lateClips(150*time.Second, 3, "x"))
	h.run(200*time.Second, 205*time.Second, steadyVoice, h.steadyChat)
	if len(h.tickErrs) == 0 || !errors.Is(h.tickErrs[0], boom) {
		t.Fatalf("tick errors = %v", h.tickErrs)
	}
	if h.repo.Len() != 0 {
		t.Fatal("clip persisted despite retain failure")
	}

	// Save failure: the retained media is deleted.
	h = newHarness(t, true)
	h.repo.SaveHook = func(*domain.Clip) error { return boom }
	h.run(0, 200*time.Second, shoutAt(150*time.Second), h.steadyChat)
	_ = h.d.ObserveClips(ctx, testLive.ID, lateClips(150*time.Second, 3, "y"))
	h.run(200*time.Second, 205*time.Second, steadyVoice, h.steadyChat)
	if len(h.capture.Deleted) != 1 || len(h.capture.Live) != 0 {
		t.Fatalf("retained media not cleaned up: deleted=%v live=%v", h.capture.Deleted, h.capture.Live)
	}
}

func TestDetectMomentSessionLifecycle(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	h.d.StartStream(testStreamer, testLive, captureStart.Add(time.Hour)) // no-op: already open
	if h.d.Active() != 1 {
		t.Fatal("expected one session")
	}
	h.d.SetViewers(testLive.ID, 5000)
	h.d.StopStream(testLive.ID)
	h.d.StopStream(testLive.ID) // idempotent
	if h.d.Active() != 0 {
		t.Fatal("session not closed")
	}
	// Unknown streams are ignored.
	h.d.ObserveAudio("nope", domain.AudioFrame{})
	h.d.ObserveChat("nope", captureStart, "x")
	h.d.SetViewers("nope", 1)
	if err := h.d.Tick(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.ObserveClips(ctx, "nope", lateClips(0, 3, "z")); err != nil {
		t.Fatal(err)
	}
}
