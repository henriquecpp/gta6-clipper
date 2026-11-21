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
// of live broadcasts in a monitored game and reports entries and exits.
//
// A live outside the game filter is treated as offline for monitoring: when a
// streamer switches to another category the live is reported as ended (the
// capture stops) and it starts again when they come back.
type MonitorLives struct {
	platform  Platform
	observer  LiveObserver
	log       *slog.Logger
	streamers map[string]domain.Streamer
	logins    []string
	games     domain.GameFilter

	mu      sync.Mutex
	live    map[string]domain.LiveStream // on air in a monitored game
	offGame map[string]domain.LiveStream // on air in another category
}

// NewMonitorLives builds the use case. Streamers with AllowsClips=false are
// ignored (logged), as required by CLAUDE.md §18. An empty games filter
// monitors every category. observer may be nil.
func NewMonitorLives(p Platform, streamers []domain.Streamer, games domain.GameFilter, observer LiveObserver, log *slog.Logger) *MonitorLives {
	m := &MonitorLives{
		platform:  p,
		observer:  observer,
		log:       log,
		streamers: make(map[string]domain.Streamer),
		games:     slices.Clone(games),
		live:      make(map[string]domain.LiveStream),
		offGame:   make(map[string]domain.LiveStream),
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
	other := make(map[string]domain.LiveStream)
	for _, l := range streams {
		login := strings.ToLower(l.Login)
		if _, ok := m.streamers[login]; !ok {
			continue
		}
		l.Login = login
		if m.games.Allows(l) {
			current[login] = l
		} else {
			other[login] = l
		}
	}

	var started, ended, updated, skipped []domain.LiveStream
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
		} else {
			updated = append(updated, cur)
		}
		m.live[login] = cur // refresh title, game and viewers
	}
	for login, cur := range other {
		// Logged once per live and category, not on every poll.
		if prev, ok := m.offGame[login]; !ok || prev.ID != cur.ID || prev.GameID != cur.GameID || prev.Game != cur.Game {
			skipped = append(skipped, cur)
		}
		m.offGame[login] = cur
	}
	for login := range m.offGame {
		if _, ok := other[login]; !ok {
			delete(m.offGame, login)
		}
	}
	m.mu.Unlock()

	byLogin := func(a, b domain.LiveStream) int { return cmp.Compare(a.Login, b.Login) }
	slices.SortFunc(ended, byLogin)
	slices.SortFunc(started, byLogin)
	slices.SortFunc(updated, byLogin)
	slices.SortFunc(skipped, byLogin)
	left := make(map[string]bool)
	for _, l := range ended {
		if o, ok := other[l.Login]; ok && o.ID == l.ID {
			left[l.Login] = true
			m.log.Info("live left the monitored games", slog.String("streamer", l.Login), slog.String("stream_id", string(l.ID)),
				slog.String("game", o.Game), slog.String("game_id", o.GameID))
		} else {
			m.log.Info("live ended", slog.String("streamer", l.Login), slog.String("stream_id", string(l.ID)))
		}
		if m.observer != nil {
			m.observer.StreamEnded(ctx, m.streamers[l.Login], l)
		}
	}
	for _, l := range skipped {
		if left[l.Login] {
			continue // already logged as leaving the monitored games
		}
		m.log.Info("live not monitored: game outside the list", slog.String("streamer", l.Login),
			slog.String("stream_id", string(l.ID)), slog.String("game", l.Game), slog.String("game_id", l.GameID))
	}
	for _, l := range started {
		m.log.Info("live started", slog.String("streamer", l.Login), slog.String("stream_id", string(l.ID)),
			slog.String("game", l.Game), slog.String("title", l.Title), slog.Int("viewers", l.Viewers))
		if m.observer != nil {
			m.observer.StreamStarted(ctx, m.streamers[l.Login], l)
		}
	}
	if m.observer != nil {
		for _, l := range updated {
			m.observer.StreamUpdated(ctx, l)
		}
	}
	return nil
}

// Live returns a snapshot of the broadcasts being monitored, by login.
func (m *MonitorLives) Live() []domain.LiveStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedLives(m.live)
}

// OffGame returns a snapshot of the allowlisted broadcasts on air in a
// category outside the games filter (not monitored), by login.
func (m *MonitorLives) OffGame() []domain.LiveStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedLives(m.offGame)
}

func sortedLives(byLogin map[string]domain.LiveStream) []domain.LiveStream {
	out := make([]domain.LiveStream, 0, len(byLogin))
	for _, l := range byLogin {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b domain.LiveStream) int { return cmp.Compare(a.Login, b.Login) })
	return out
}
