package twitchingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Capturer runs the media capture of one live broadcast until ctx is
// cancelled or the stream ends, writing the circular buffer and emitting
// VAD-classified audio frames positioned on the stream timeline.
type Capturer interface {
	Capture(ctx context.Context, live domain.LiveStream, captureStart time.Time, onFrame func(domain.AudioFrame)) error
}

// SupervisorConfig configures the per-live loops.
type SupervisorConfig struct {
	Tick             time.Duration
	ClipPollInterval time.Duration
	// ClipLookback is how far back each clip poll looks (clips appear late).
	ClipLookback time.Duration
	MaxStreams   int
	// RestartBackoff bounds the delay between capture restarts.
	RestartBackoffMin time.Duration
	RestartBackoffMax time.Duration
}

// Supervisor implements app.LiveObserver: for each live broadcast it runs the
// detection tick, the viewer clip polling and (when configured) the capture,
// and routes chat messages from the shared EventSub session.
type Supervisor struct {
	cfg      SupervisorConfig
	detect   *app.DetectMoment
	platform app.Platform
	capture  Capturer
	chat     *ChatClient
	log      *slog.Logger
	now      func() time.Time

	cmds chan command

	mu            sync.Mutex
	byBroadcaster map[string]domain.StreamID
	running       int
}

type commandKind int

const (
	cmdStart commandKind = iota
	cmdStop
)

type command struct {
	kind     commandKind
	streamer domain.Streamer
	live     domain.LiveStream
}

var _ app.LiveObserver = (*Supervisor)(nil)

// SupervisorOption configures optional parts of the supervisor.
type SupervisorOption func(*Supervisor)

// WithChat enables the chat signal through an EventSub session.
func WithChat(sub ChatSubscriber, opts ...ChatOption) SupervisorOption {
	return func(s *Supervisor) {
		opts = append([]ChatOption{WithChatLogger(s.log)}, opts...)
		s.chat = NewChatClient(sub, s.onChat, opts...)
	}
}

// WithCapturer enables media capture and the voice signal.
func WithCapturer(c Capturer) SupervisorOption { return func(s *Supervisor) { s.capture = c } }

// NewSupervisor builds a supervisor.
func NewSupervisor(cfg SupervisorConfig, d *app.DetectMoment, p app.Platform, log *slog.Logger, opts ...SupervisorOption) *Supervisor {
	if cfg.Tick <= 0 {
		cfg.Tick = 500 * time.Millisecond
	}
	if cfg.ClipPollInterval <= 0 {
		cfg.ClipPollInterval = 30 * time.Second
	}
	if cfg.ClipLookback <= 0 {
		cfg.ClipLookback = 10 * time.Minute
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 8
	}
	if cfg.RestartBackoffMin <= 0 {
		cfg.RestartBackoffMin = 2 * time.Second
	}
	if cfg.RestartBackoffMax <= 0 {
		cfg.RestartBackoffMax = time.Minute
	}
	s := &Supervisor{
		cfg: cfg, detect: d, platform: p, log: log,
		now:           func() time.Time { return time.Now().UTC() },
		cmds:          make(chan command, 64),
		byBroadcaster: make(map[string]domain.StreamID),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// StreamStarted schedules the start of a live (non-blocking).
func (s *Supervisor) StreamStarted(_ context.Context, st domain.Streamer, live domain.LiveStream) {
	s.send(command{kind: cmdStart, streamer: st, live: live})
}

// StreamUpdated refreshes the viewer count of a running live.
func (s *Supervisor) StreamUpdated(_ context.Context, live domain.LiveStream) {
	s.detect.SetViewers(live.ID, live.Viewers)
}

// StreamEnded schedules the stop of a live (non-blocking).
func (s *Supervisor) StreamEnded(_ context.Context, st domain.Streamer, live domain.LiveStream) {
	s.send(command{kind: cmdStop, streamer: st, live: live})
}

func (s *Supervisor) send(c command) {
	select {
	case s.cmds <- c:
	default:
		s.log.Error("supervisor command queue full; dropping", slog.String("stream_id", string(c.live.ID)))
	}
}

// Running returns how many lives are being processed.
func (s *Supervisor) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Supervisor) onChat(broadcasterID string, receivedAt time.Time, text string) {
	s.mu.Lock()
	id, ok := s.byBroadcaster[broadcasterID]
	s.mu.Unlock()
	if ok {
		s.detect.ObserveChat(id, receivedAt, text)
	}
}

type runningStream struct {
	live   domain.LiveStream
	cancel context.CancelFunc
	done   chan struct{}
}

// Run owns every per-live goroutine and the chat session; it returns after
// all of them have stopped once ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	if s.chat != nil {
		g.Go(func() error { return s.chat.Run(gctx) })
	}
	running := map[domain.StreamID]*runningStream{}
	stop := func(id domain.StreamID) {
		r, ok := running[id]
		if !ok {
			return
		}
		r.cancel()
		<-r.done
		delete(running, id)
		s.detect.StopStream(id)
		if s.chat != nil {
			s.chat.Leave(r.live.BroadcasterID)
		}
		s.mu.Lock()
		delete(s.byBroadcaster, r.live.BroadcasterID)
		s.running = len(running)
		s.mu.Unlock()
		s.log.Info("live supervisor stopped", slog.String("stream_id", string(id)))
	}

loop:
	for {
		select {
		case <-gctx.Done():
			break loop
		case c := <-s.cmds:
			switch c.kind {
			case cmdStart:
				if _, ok := running[c.live.ID]; ok {
					continue
				}
				if len(running) >= s.cfg.MaxStreams {
					s.log.Warn("max_streams reached; live not monitored", slog.String("streamer", c.streamer.Login),
						slog.Int("max_streams", s.cfg.MaxStreams))
					continue
				}
				captureStart := s.now()
				s.detect.StartStream(c.streamer, c.live, captureStart)
				sctx, cancel := context.WithCancel(gctx)
				r := &runningStream{live: c.live, cancel: cancel, done: make(chan struct{})}
				running[c.live.ID] = r
				s.mu.Lock()
				s.byBroadcaster[c.live.BroadcasterID] = c.live.ID
				s.running = len(running)
				s.mu.Unlock()
				if s.chat != nil {
					s.chat.Join(c.live.BroadcasterID)
				}
				st, live := c.streamer, c.live
				g.Go(func() error {
					defer close(r.done)
					s.runStream(sctx, st, live, captureStart)
					return nil
				})
				s.log.Info("live supervisor started", slog.String("streamer", st.Login), slog.String("stream_id", string(live.ID)))
			case cmdStop:
				stop(c.live.ID)
			}
		}
	}
	for id := range running {
		stop(id)
	}
	return g.Wait()
}

// runStream runs the loops of one live until ctx is cancelled.
func (s *Supervisor) runStream(ctx context.Context, st domain.Streamer, live domain.LiveStream, captureStart time.Time) {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		s.every(ctx, s.cfg.Tick, func() {
			if err := s.detect.Tick(ctx, live.ID); err != nil && ctx.Err() == nil {
				s.log.Warn("detection tick failed", slog.String("streamer", st.Login), slog.String("err", err.Error()))
			}
		})
		return nil
	})
	if live.BroadcasterID != "" && s.platform != nil {
		g.Go(func() error {
			s.every(ctx, s.cfg.ClipPollInterval, func() { s.pollClips(ctx, st, live, captureStart) })
			return nil
		})
	}
	if s.capture != nil {
		g.Go(func() error {
			s.captureLoop(ctx, st, live, captureStart)
			return nil
		})
	}
	_ = g.Wait() // loops only return nil
}

func (s *Supervisor) pollClips(ctx context.Context, st domain.Streamer, live domain.LiveStream, captureStart time.Time) {
	// Clips from before the capture started never become moments, but they
	// give the channel's clip rate (the clip-rain baseline) from the first poll.
	since := s.now().Add(-s.cfg.ClipLookback)
	clips, err := s.platform.RecentClips(ctx, live.BroadcasterID, since)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("poll viewer clips failed", slog.String("streamer", st.Login), slog.String("err", err.Error()))
		}
		return
	}
	if err := s.detect.ObserveClips(ctx, live.ID, clips); err != nil && ctx.Err() == nil {
		s.log.Warn("observe viewer clips failed", slog.String("streamer", st.Login), slog.String("err", err.Error()))
	}
}

// captureLoop keeps the capture running, restarting it with exponential
// backoff when it exits before ctx is cancelled (stream hiccup, ad break...).
func (s *Supervisor) captureLoop(ctx context.Context, st domain.Streamer, live domain.LiveStream, captureStart time.Time) {
	backoff := s.cfg.RestartBackoffMin
	onFrame := func(f domain.AudioFrame) { s.detect.ObserveAudio(live.ID, f) }
	for {
		started := s.now()
		err := s.capture.Capture(ctx, live, captureStart, onFrame)
		if ctx.Err() != nil {
			return
		}
		if s.now().Sub(started) > 5*time.Minute {
			backoff = s.cfg.RestartBackoffMin
		}
		s.log.Warn("capture stopped; restarting", slog.String("streamer", st.Login), slog.String("stream_id", string(live.ID)),
			slog.String("err", errString(err)), slog.Duration("backoff", backoff))
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, s.cfg.RestartBackoffMax)
	}
}

// every runs fn immediately and then at each interval until ctx is done.
func (s *Supervisor) every(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	fn()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
