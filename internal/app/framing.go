package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// lastFacecamTTL is how long a facecam position found in an earlier clip is
// trusted when a new clip has no clear one (a cutscene, the streamer away).
const lastFacecamTTL = time.Hour

// Framing decides the vertical framing of a clip. For streamers with the
// auto layout it looks for the facecam in the clip itself, so a streamer who
// moves the camera is followed from the next clip on; when none is found it
// falls back to the last position found, the configured stack regions or
// blur. Other layouts come straight from the configuration.
type Framing struct {
	detector  FacecamDetector
	streamers map[string]domain.Streamer
	clock     Clock
	log       *slog.Logger
	metrics   Metrics

	mu   sync.Mutex
	last map[string]seenFacecam
}

type seenFacecam struct {
	rect domain.Rect
	at   time.Time
}

// NewFraming builds the component; detector may be nil (auto then uses the
// fallbacks only) and metrics may be nil.
func NewFraming(detector FacecamDetector, streamers map[string]domain.Streamer, clock Clock, log *slog.Logger, metrics Metrics) *Framing {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	return &Framing{detector: detector, streamers: streamers, clock: clock, log: log, metrics: metrics, last: map[string]seenFacecam{}}
}

// Resolve returns the layout and regions to render the clip with.
func (f *Framing) Resolve(ctx context.Context, c *domain.Clip) (domain.Layout, domain.StackRegions) {
	st := f.streamers[c.Streamer]
	if st.Layout != domain.LayoutAuto {
		return layoutOf(f.streamers, c.Streamer)
	}
	gameplay := domain.FullFrame
	if st.Stack.Gameplay.Valid() {
		gameplay = st.Stack.Gameplay
	}
	log := f.log.With(slog.String("clip_id", string(c.ID)), slog.String("streamer", c.Streamer))
	if f.detector != nil {
		t0 := f.clock.Now()
		r, ok, err := f.detector.Detect(ctx, c.SourceMedia)
		f.metrics.StageDuration("facecam", f.clock.Now().Sub(t0))
		switch {
		case err != nil:
			log.Warn("facecam detection failed; using the fallback framing", slog.String("err", err.Error()))
		case ok && r.Valid():
			f.mu.Lock()
			f.last[c.Streamer] = seenFacecam{rect: r, at: f.clock.Now()}
			f.mu.Unlock()
			log.Info("facecam found", slog.Float64("x", r.X), slog.Float64("y", r.Y), slog.Float64("w", r.W), slog.Float64("h", r.H))
			return domain.LayoutStack, domain.StackRegions{Facecam: r, Gameplay: gameplay}
		default:
			log.Info("no facecam found in the clip")
		}
	}
	f.mu.Lock()
	seen, ok := f.last[c.Streamer]
	f.mu.Unlock()
	if ok && f.clock.Now().Sub(seen.at) < lastFacecamTTL {
		return domain.LayoutStack, domain.StackRegions{Facecam: seen.rect, Gameplay: gameplay}
	}
	return layoutOf(f.streamers, c.Streamer)
}
