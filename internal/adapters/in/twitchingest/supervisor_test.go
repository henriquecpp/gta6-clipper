package twitchingest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/domain/detect"
	"github.com/henriquecpp/gta6-clipper/internal/platform/clock"
)

type countingPlatform struct {
	fakes.Platform
	clipCalls atomic.Int32
}

func (p *countingPlatform) RecentClips(ctx context.Context, b string, since time.Time) ([]domain.ViewerClip, error) {
	p.clipCalls.Add(1)
	return p.Platform.RecentClips(ctx, b, since)
}

type fakeCapturer struct {
	mu     sync.Mutex
	starts int
	fail   int // number of starts that return an error immediately
}

func (f *fakeCapturer) Capture(ctx context.Context, live domain.LiveStream, start time.Time, onFrame func(domain.AudioFrame)) error {
	f.mu.Lock()
	f.starts++
	n := f.starts
	f.mu.Unlock()
	if n <= f.fail {
		return errors.New("stream offline")
	}
	onFrame(domain.AudioFrame{Pos: 0, DBFS: -30, Speech: true})
	<-ctx.Done()
	return ctx.Err()
}

func newDetect() *app.DetectMoment {
	cfg := app.DetectionConfig{
		Scorer:         detect.ScorerConfig{Weights: map[domain.SignalKind]float64{domain.SignalVoice: 1, domain.SignalChat: 1}, ThresholdOn: 3, ThresholdOff: 1, ZMax: 6, MinSignalZ: 2},
		PreRoll:        time.Minute,
		PostRoll:       30 * time.Second,
		BufferDuration: 10 * time.Minute,
	}
	return app.NewDetectMoment(cfg, fakes.NewClipRepo(), func() (domain.ClipID, error) { return "x", nil }, clock.Real{}, slog.New(slog.DiscardHandler))
}

// TestSupervisorLifecycle runs inside a synctest bubble: time is virtual and
// the test fails if any goroutine is left running (no goroutine leak).
func TestSupervisorLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newDetect()
		p := &countingPlatform{}
		capt := &fakeCapturer{fail: 2}
		s := NewSupervisor(SupervisorConfig{Tick: time.Second, ClipPollInterval: 30 * time.Second, MaxStreams: 1,
			RestartBackoffMin: time.Second, RestartBackoffMax: 4 * time.Second},
			d, p, slog.New(slog.DiscardHandler), WithCapturer(capt))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		st := domain.Streamer{Login: "a", Language: "en", AllowsClips: true}
		live := domain.LiveStream{ID: "s1", BroadcasterID: "b1", Login: "a", Viewers: 10}
		s.StreamStarted(ctx, st, live)
		s.StreamStarted(ctx, st, live)                                                                      // duplicate start ignored
		s.StreamStarted(ctx, domain.Streamer{Login: "b"}, domain.LiveStream{ID: "s2", BroadcasterID: "b2"}) // over max_streams
		time.Sleep(61 * time.Second)
		synctest.Wait()

		if s.Running() != 1 || d.Active() != 1 {
			t.Fatalf("running=%d active=%d, want 1/1", s.Running(), d.Active())
		}
		if n := p.clipCalls.Load(); n != 3 { // immediately, +30 s, +60 s
			t.Errorf("clip polls = %d, want 3", n)
		}
		capt.mu.Lock()
		starts := capt.starts
		capt.mu.Unlock()
		if starts != 3 { // two failures restarted with backoff, third keeps running
			t.Errorf("capture starts = %d, want 3", starts)
		}
		s.StreamUpdated(ctx, domain.LiveStream{ID: "s1", Viewers: 99})

		s.StreamEnded(ctx, st, live)
		time.Sleep(time.Second)
		synctest.Wait()
		if s.Running() != 0 || d.Active() != 0 {
			t.Fatalf("after end: running=%d active=%d", s.Running(), d.Active())
		}

		// A live still running when the process stops is shut down too.
		s.StreamStarted(ctx, st, domain.LiveStream{ID: "s3", BroadcasterID: "b1"})
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run = %v", err)
		}
		if d.Active() != 0 {
			t.Fatal("detection sessions left open after shutdown")
		}
	})
}

func TestSupervisorRoutesChat(t *testing.T) {
	d := newDetect()
	s := NewSupervisor(SupervisorConfig{}, d, nil, slog.New(slog.DiscardHandler))
	s.byBroadcaster["b1"] = "s1"
	s.onChat("b1", time.Now(), "hi")   // routed (session absent: ignored by DetectMoment)
	s.onChat("zz", time.Now(), "lost") // unknown broadcaster: dropped
	for range 100 {
		s.send(command{kind: cmdStop})
	}
	if len(s.cmds) != cap(s.cmds) {
		t.Fatalf("queue = %d", len(s.cmds))
	}
}
