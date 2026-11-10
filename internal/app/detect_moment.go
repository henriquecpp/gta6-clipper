package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/domain/detect"
)

// DetectionConfig configures DetectMoment.
type DetectionConfig struct {
	// Tick is the evaluation step of the scorer (e.g. 500 ms).
	Tick   time.Duration
	Scorer detect.ScorerConfig
	Voice  detect.VoiceConfig
	// Chat.Terms is replaced per streamer by TermsByLanguage[streamer.Language].
	Chat            detect.ChatConfig
	TermsByLanguage map[string]map[string]float64
	Clips           detect.ClipsConfig
	PreRoll         time.Duration
	PostRoll        time.Duration
	// BufferDuration is how much media the capture buffer keeps.
	BufferDuration time.Duration
	// RetainMargin delays the retention past the post-roll so the segment
	// holding the end of the window is complete (e.g. one segment + 1 s).
	RetainMargin time.Duration
}

// IDFunc generates clip ids (UUID v7, injected by cmd).
type IDFunc func() (domain.ClipID, error)

// ClipHandler is called after a detected clip is persisted (SendPreview).
type ClipHandler func(ctx context.Context, c *domain.Clip)

// DetectMoment scores the signals of every live broadcast and turns accepted
// moments into clips: it retains the media window, persists the clip in
// `detected` and triggers the preview. It costs zero AI tokens.
type DetectMoment struct {
	cfg     DetectionConfig
	repo    ClipRepository
	capture MediaCapture
	onClip  ClipHandler
	newID   IDFunc
	clock   Clock
	log     *slog.Logger
	metrics Metrics

	mu       sync.Mutex
	sessions map[domain.StreamID]*session
}

// DetectMomentOption configures optional collaborators.
type DetectMomentOption func(*DetectMoment)

// WithCapture enables media retention. Without it, accepted moments are only
// logged (Phase 2 mode, or when capture is unavailable).
func WithCapture(c MediaCapture) DetectMomentOption { return func(d *DetectMoment) { d.capture = c } }

// WithClipHandler sets the callback run after a clip is persisted.
func WithClipHandler(h ClipHandler) DetectMomentOption { return func(d *DetectMoment) { d.onClip = h } }

// WithDetectMetrics sets the metrics sink.
func WithDetectMetrics(m Metrics) DetectMomentOption { return func(d *DetectMoment) { d.metrics = m } }

// NewDetectMoment builds the use case.
func NewDetectMoment(cfg DetectionConfig, repo ClipRepository, newID IDFunc, clock Clock, log *slog.Logger, opts ...DetectMomentOption) *DetectMoment {
	if cfg.Tick <= 0 {
		cfg.Tick = 500 * time.Millisecond
	}
	retention := cfg.BufferDuration + cfg.PreRoll + cfg.PostRoll
	cfg.Voice.Retention, cfg.Chat.Retention, cfg.Clips.Retention = retention, retention, retention
	if cfg.Scorer.MaxEpisode <= 0 {
		cfg.Scorer.MaxEpisode = cfg.PostRoll
	}
	d := &DetectMoment{
		cfg: cfg, repo: repo, newID: newID, clock: clock, log: log, metrics: NopMetrics{},
		sessions: make(map[domain.StreamID]*session),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

type acceptedMoment struct {
	peak   domain.StreamPos
	clipID domain.ClipID
	clips  int
}

type session struct {
	mu       sync.Mutex
	streamer domain.Streamer
	live     domain.LiveStream
	tl       domain.Timeline
	voice    *detect.Voice
	chat     *detect.Chat
	clips    *detect.Clips
	scorer   *detect.Scorer
	lag      time.Duration

	next     domain.StreamPos // next evaluation position
	pending  []domain.Moment  // waiting for the post-roll to be captured
	accepted []acceptedMoment
}

// StartStream opens a detection session; captureStart is the wall time of
// StreamPos 0. Starting an already open stream is a no-op.
func (d *DetectMoment) StartStream(s domain.Streamer, live domain.LiveStream, captureStart time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sessions[live.ID]; ok {
		return
	}
	chatCfg := d.cfg.Chat
	chatCfg.Terms = d.cfg.TermsByLanguage[strings.ToLower(s.Language)]
	sess := &session{
		streamer: s,
		live:     live,
		tl:       domain.Timeline{CaptureStart: captureStart, StreamStart: live.StartedAt, Offsets: s.Offsets},
		voice:    detect.NewVoice(d.cfg.Voice),
		chat:     detect.NewChat(chatCfg),
		clips:    detect.NewClips(d.cfg.Clips),
		// The chat of an event arrives (chat − capture_latency) after its video.
		lag: max(0, s.Offsets.Chat-s.Offsets.CaptureLatency) + time.Second,
	}
	sess.chat.SetViewers(live.Viewers)
	sess.scorer = detect.NewScorer(d.cfg.Scorer, sess.voice, sess.chat, sess.clips)
	d.sessions[live.ID] = sess
	d.log.Info("detection started", slog.String("streamer", s.Login), slog.String("stream_id", string(live.ID)))
}

// StopStream closes a session; moments still waiting for their post-roll are dropped.
func (d *DetectMoment) StopStream(id domain.StreamID) {
	d.mu.Lock()
	sess, ok := d.sessions[id]
	delete(d.sessions, id)
	d.mu.Unlock()
	if !ok {
		return
	}
	sess.mu.Lock()
	dropped := len(sess.pending)
	sess.mu.Unlock()
	d.log.Info("detection stopped", slog.String("streamer", sess.streamer.Login), slog.String("stream_id", string(id)),
		slog.Int("dropped_pending", dropped))
}

func (d *DetectMoment) session(id domain.StreamID) *session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[id]
}

// Active returns how many sessions are open.
func (d *DetectMoment) Active() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

// ObserveAudio feeds one VAD-classified audio frame.
func (d *DetectMoment) ObserveAudio(id domain.StreamID, f domain.AudioFrame) {
	if s := d.session(id); s != nil {
		s.mu.Lock()
		s.voice.Observe(f)
		s.mu.Unlock()
	}
}

// ObserveChat feeds one chat message received at wall time receivedAt.
func (d *DetectMoment) ObserveChat(id domain.StreamID, receivedAt time.Time, text string) {
	if s := d.session(id); s != nil {
		s.mu.Lock()
		s.chat.Observe(s.tl.ChatPos(receivedAt), text)
		s.mu.Unlock()
	}
}

// SetViewers updates the viewer count used to normalize the chat.
func (d *DetectMoment) SetViewers(id domain.StreamID, viewers int) {
	if s := d.session(id); s != nil {
		s.mu.Lock()
		s.chat.SetViewers(viewers)
		s.live.Viewers = viewers
		s.mu.Unlock()
	}
}

// Tick evaluates the scorer up to the current position (minus the chat lag)
// and accepts the moments whose post-roll has been captured.
func (d *DetectMoment) Tick(ctx context.Context, id domain.StreamID) error {
	s := d.session(id)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	capPos := s.tl.CapturePos(d.clock.Now())
	evalTo := capPos - domain.StreamPos(s.lag)
	step := domain.StreamPos(d.cfg.Tick)
	if s.next == 0 || evalTo-s.next > domain.StreamPos(d.cfg.BufferDuration) {
		s.next = max(0, evalTo-domain.StreamPos(d.cfg.BufferDuration))
		s.next -= s.next % step
	}
	for ; s.next <= evalTo; s.next += step {
		if m, ok := s.scorer.Evaluate(s.next); ok {
			s.pending = append(s.pending, m)
		}
	}
	if _, values := s.scorer.Last(); values != nil {
		for k, v := range values {
			d.metrics.SignalZ(s.streamer.Login, k, v)
		}
	}
	var ready []domain.Moment
	keep := s.pending[:0]
	for _, m := range s.pending {
		if m.Peak+domain.StreamPos(d.cfg.PostRoll+d.cfg.RetainMargin) <= capPos {
			ready = append(ready, m)
		} else {
			keep = append(keep, m)
		}
	}
	s.pending = keep
	s.mu.Unlock()

	var errs []error
	for _, m := range ready {
		if err := d.accept(ctx, s, m, capPos); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// accept retains the window, persists the clip and triggers the preview.
func (d *DetectMoment) accept(ctx context.Context, s *session, m domain.Moment, capPos domain.StreamPos) error {
	w, ok := domain.RetentionWindow(m.Peak, d.cfg.PreRoll, d.cfg.PostRoll)
	bufferStart := capPos - domain.StreamPos(d.cfg.BufferDuration)
	if !ok || m.Peak < bufferStart {
		d.log.Warn("moment dropped: window outside the buffer", slog.String("streamer", s.streamer.Login),
			slog.String("peak", m.Peak.String()))
		return nil
	}
	w.From = max(w.From, bufferStart)

	s.mu.Lock()
	live := s.live
	peakAt := s.tl.WallTime(m.Peak)
	liveOffset := time.Duration(0)
	if !s.tl.StreamStart.IsZero() {
		liveOffset = max(0, peakAt.Sub(s.tl.StreamStart))
	}
	s.mu.Unlock()

	attrs := []any{
		slog.String("streamer", s.streamer.Login), slog.String("stream_id", string(live.ID)),
		slog.String("peak", m.Peak.String()), slog.Float64("score", round2(m.Score)),
		slog.Float64("voice", round2(m.Values[domain.SignalVoice])), slog.Float64("chat", round2(m.Values[domain.SignalChat])),
		slog.Float64("clips", m.Values[domain.SignalClips]), slog.Bool("late", m.Late),
	}
	d.log.Info("moment detected", attrs...)
	d.metrics.MomentDetected(s.streamer.Login)
	if d.capture == nil {
		return nil
	}

	start := d.clock.Now()
	ref, retained, err := d.capture.RetainWindow(ctx, live.ID, w.From, w.To)
	d.metrics.StageDuration("retain", d.clock.Now().Sub(start))
	if err != nil {
		return fmt.Errorf("retain window for %s at %s: %w", s.streamer.Login, m.Peak, err)
	}
	if retained.Valid() && retained.Contains(m.Peak) {
		w = retained // whole segments: the clip records what was actually kept
	}
	s.mu.Lock()
	timeline := s.signalTimeline(w)
	s.mu.Unlock()
	id, err := d.newID()
	if err != nil {
		d.deleteMedia(ctx, ref)
		return fmt.Errorf("new clip id: %w", err)
	}
	clip, err := domain.NewClip(domain.NewClipParams{
		ID: id, Streamer: s.streamer.Login, Live: live, Moment: m, Window: w, Timeline: timeline, Source: ref,
		PeakAt: peakAt, LiveOffset: liveOffset,
	}, d.clock.Now())
	if err != nil {
		d.deleteMedia(ctx, ref)
		return err
	}
	if err := d.repo.Save(ctx, clip); err != nil {
		d.deleteMedia(ctx, ref)
		return fmt.Errorf("save clip %s: %w", id, err)
	}
	d.metrics.ClipState(domain.StateDetected)

	s.mu.Lock()
	s.accepted = append(s.accepted, acceptedMoment{peak: m.Peak, clipID: id, clips: m.ViewerClips})
	s.mu.Unlock()
	d.log.Info("clip detected", append(attrs, slog.String("clip_id", string(id)))...)
	if d.onClip != nil {
		d.onClip(ctx, clip)
	}
	return nil
}

func (d *DetectMoment) deleteMedia(ctx context.Context, ref domain.MediaRef) {
	if err := d.capture.Delete(ctx, ref); err != nil {
		d.log.Warn("delete retained media", slog.String("ref", string(ref)), slog.String("err", err.Error()))
	}
}

// signalTimeline samples every detector once per second over w (caller holds s.mu).
func (s *session) signalTimeline(w domain.Window) []domain.SignalPoint {
	var out []domain.SignalPoint
	first := w.From - w.From%domain.StreamPos(time.Second)
	if first < w.From {
		first += domain.StreamPos(time.Second)
	}
	for p := first; p <= w.To; p += domain.StreamPos(time.Second) {
		out = append(out, domain.SignalPoint{
			Pos:   p,
			Voice: round2(s.voice.Value(p)),
			Chat:  round2(s.chat.Value(p)),
			Clips: s.clips.Value(p),
		})
	}
	return out
}

// ObserveClips feeds viewer clips (the late signal). A cluster of clips near
// a known moment raises its score; a cluster far from every known moment and
// still inside the buffer becomes a late moment.
func (d *DetectMoment) ObserveClips(ctx context.Context, id domain.StreamID, clips []domain.ViewerClip) error {
	s := d.session(id)
	if s == nil || len(clips) == 0 {
		return nil
	}
	type boost struct {
		clipID domain.ClipID
		count  int
	}
	var boosts []boost
	gw := domain.StreamPos(d.cfg.Clips.GroupWindow)
	zmax := s.scorer.ZMax()
	wClips := s.scorer.Weight(domain.SignalClips)

	s.mu.Lock()
	capPos := s.tl.CapturePos(d.clock.Now())
	s.clips.Advance(capPos)
	added := 0
	for _, c := range clips {
		if s.clips.Observe(c.ID, s.tl.ClipPos(c)) {
			added++
		}
	}
	if added > 0 {
		for _, cl := range s.clips.Clusters() {
			value := min(float64(cl.Count), zmax)
			handled := false
			for i := range s.pending {
				if absPos(s.pending[i].Peak-cl.Center) <= gw && cl.Count > s.pending[i].ViewerClips {
					m := &s.pending[i]
					m.Score += wClips * (value - m.Values[domain.SignalClips])
					m.Values[domain.SignalClips] = value
					m.ViewerClips = cl.Count
				}
				if absPos(s.pending[i].Peak-cl.Center) <= gw {
					handled = true
				}
			}
			for i := range s.accepted {
				a := &s.accepted[i]
				if absPos(a.peak-cl.Center) <= gw {
					handled = true
					if cl.Count > a.clips {
						a.clips = cl.Count
						boosts = append(boosts, boost{clipID: a.clipID, count: cl.Count})
					}
				}
			}
			if handled {
				continue
			}
			last, hasLast := s.scorer.LastPeak()
			inCooldown := hasLast && absPos(cl.Center-last) < domain.StreamPos(d.cfg.Scorer.Cooldown)
			inBuffer := cl.Center-domain.StreamPos(d.cfg.PreRoll) >= capPos-domain.StreamPos(d.cfg.BufferDuration)
			if inCooldown || !inBuffer {
				continue
			}
			score, values := s.scorer.Score(cl.Center)
			values[domain.SignalClips] = value
			score += wClips * (value - min(s.clips.Value(cl.Center), zmax))
			m := domain.Moment{Peak: cl.Center, Score: score, Values: values, ViewerClips: cl.Count, Late: true}
			s.pending = append(s.pending, m)
			s.scorer.NotePeak(cl.Center)
			d.log.Info("late moment from viewer clips", slog.String("streamer", s.streamer.Login),
				slog.String("peak", cl.Center.String()), slog.Int("clips", cl.Count))
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, b := range boosts {
		if err := d.boostClip(ctx, b.clipID, b.count, zmax, wClips); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// boostClip records a confirmed viewer clip count on an already persisted clip.
func (d *DetectMoment) boostClip(ctx context.Context, id domain.ClipID, count int, zmax, w float64) error {
	return d.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		value := min(float64(count), zmax)
		if c.Moment.Values == nil {
			c.Moment.Values = map[domain.SignalKind]float64{}
		}
		c.Moment.Score += w * (value - c.Moment.Values[domain.SignalClips])
		c.Moment.Values[domain.SignalClips] = value
		c.Moment.ViewerClips = count
		d.log.Info("clip confirmed by viewer clips", slog.String("clip_id", string(id)), slog.Int("clips", count))
		return repo.Save(ctx, c)
	})
}

func absPos(p domain.StreamPos) domain.StreamPos {
	if p < 0 {
		return -p
	}
	return p
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
