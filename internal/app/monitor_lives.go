package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// MonitorLives polls the platform for the allowlisted streamers, keeps the set
// of live broadcasts and reports entries and exits.
type MonitorLives struct {
	platform  Platform
	observer  LiveObserver
	log       *slog.Logger
	streamers map[string]domain.Streamer
	logins    []string

	mu   sync.Mutex
	live map[string]domain.LiveStream
}

// NewMonitorLives builds the use case. Streamers with AllowsClips=false are
// ignored (logged), as required by CLAUDE.md §18. observer may be nil.
func NewMonitorLives(p Platform, streamers []domain.Streamer, observer LiveObserver, log *slog.Logger) *MonitorLives {
	m := &MonitorLives{
		platform:  p,
		observer:  observer,
		log:       log,
		streamers: make(map[string]domain.Streamer),
		live:      make(map[string]domain.LiveStream),
	}
	for _, s := range streamers {
		login := strings.ToLower(s.Login)
		if !s.AllowsClips {
			log.Warn("streamer ignored: allows_clips is false", slog.String("streamer", login))
			continue
		}
		if _, dup := m.streamers[login]; dup {
			continue
		}
		s.Login = login
		m.streamers[login] = s
		m.logins = append(m.logins, login)
	}
	slices.Sort(m.logins)
	return m
}

// Logins returns the monitored logins.
func (m *MonitorLives) Logins() []string { return slices.Clone(m.logins) }

// Poll queries the platform once and reports changes. On error the known set
// is kept, so a transient failure never ends a live.
func (m *MonitorLives) Poll(ctx context.Context) error {
	if len(m.logins) == 0 {
		return nil
	}
	streams, err := m.platform.LiveStreams(ctx, m.logins)
	if err != nil {
		return fmt.Errorf("poll live streams: %w", err)
	}
	current := make(map[string]domain.LiveStream, len(streams))
	for _, l := range streams {
		login := strings.ToLower(l.Login)
		if _, ok := m.streamers[login]; !ok {
			continue
		}
		l.Login = login
		current[login] = l
	}

	var started, ended []domain.LiveStream
	m.mu.Lock()
	for login, prev := range m.live {
		if cur, ok := current[login]; !ok || cur.ID != prev.ID {
			ended = append(ended, prev)
			delete(m.live, login)
		}
	}
	for login, cur := range current {
		if prev, ok := m.live[login]; !ok || prev.ID != cur.ID {
			started = append(started, cur)
		}
		m.live[login] = cur // refresh title, game and viewers
	}
	m.mu.Unlock()

	byLogin := func(a, b domain.LiveStream) int { return cmp.Compare(a.Login, b.Login) }
	slices.SortFunc(ended, byLogin)
	slices.SortFunc(started, byLogin)
	for _, l := range ended {
		m.log.Info("live ended", slog.String("streamer", l.Login), slog.String("stream_id", string(l.ID)))
		if m.observer != nil {
			m.observer.StreamEnded(ctx, m.streamers[l.Login], l)
		}
	}
	for _, l := range started {
		m.log.Info("live started", slog.String("streamer", l.Login), slog.String("stream_id", string(l.ID)),
			slog.String("game", l.Game), slog.String("title", l.Title), slog.Int("viewers", l.Viewers))
		if m.observer != nil {
			m.observer.StreamStarted(ctx, m.streamers[l.Login], l)
		}
	}
	return nil
}

// Live returns a snapshot of the broadcasts currently on air, by login.
func (m *MonitorLives) Live() []domain.LiveStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.LiveStream, 0, len(m.live))
	for _, l := range m.live {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b domain.LiveStream) int { return cmp.Compare(a.Login, b.Login) })
	return out
}
