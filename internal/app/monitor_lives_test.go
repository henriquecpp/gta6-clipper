package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestMonitorLivesEntriesAndExits(t *testing.T) {
	ctx := context.Background()
	p := &fakes.Platform{}
	obs := &fakes.LiveObserver{}
	streamers := []domain.Streamer{
		{Login: "Alice", AllowsClips: true, Layout: domain.LayoutBlur},
		{Login: "bob", AllowsClips: true, Layout: domain.LayoutBlur},
		{Login: "carol", AllowsClips: false, Layout: domain.LayoutBlur},
		{Login: "alice", AllowsClips: true, Layout: domain.LayoutBlur}, // duplicate
	}
	m := app.NewMonitorLives(p, streamers, nil, obs, discardLog())
	if got := m.Logins(); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("logins = %v (carol must be ignored, alice deduplicated)", got)
	}

	// Carol is live but not monitored; alice goes live.
	p.SetStreams(domain.LiveStream{ID: "s1", Login: "alice", Viewers: 10}, domain.LiveStream{ID: "s9", Login: "carol"})
	if err := m.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	started, ended := obs.Snapshot()
	if len(started) != 1 || started[0].ID != "s1" || len(ended) != 0 {
		t.Fatalf("after poll 1: started=%v ended=%v", started, ended)
	}

	// Same stream again: no new notifications, metadata refreshed.
	p.SetStreams(domain.LiveStream{ID: "s1", Login: "alice", Viewers: 50}, domain.LiveStream{ID: "s2", Login: "BOB"})
	if err := m.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	started, _ = obs.Snapshot()
	if len(started) != 2 || started[1].Login != "bob" {
		t.Fatalf("after poll 2: started=%v", started)
	}
	live := m.Live()
	if len(live) != 2 || live[0].Viewers != 50 {
		t.Fatalf("live = %+v", live)
	}
	if len(obs.Updated) != 1 || obs.Updated[0].Viewers != 50 {
		t.Fatalf("updated = %+v", obs.Updated)
	}

	// Transient error keeps the known set.
	p.SetStreamErr(errors.New("timeout"))
	if err := m.Poll(ctx); err == nil {
		t.Fatal("expected error")
	}
	if len(m.Live()) != 2 {
		t.Fatal("error must not end lives")
	}
	p.SetStreamErr(nil)

	// Alice restarts (new stream id) and bob goes offline.
	p.SetStreams(domain.LiveStream{ID: "s3", Login: "alice"})
	if err := m.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	started, ended = obs.Snapshot()
	if len(ended) != 2 || ended[0].ID != "s1" || ended[1].ID != "s2" {
		t.Fatalf("ended = %v", ended)
	}
	if len(started) != 3 || started[2].ID != "s3" {
		t.Fatalf("started = %v", started)
	}
}

func TestMonitorLivesNoStreamersAndNilObserver(t *testing.T) {
	p := &fakes.Platform{}
	m := app.NewMonitorLives(p, nil, nil, nil, discardLog())
	if err := m.Poll(context.Background()); err != nil || p.Calls != 0 {
		t.Fatalf("poll with no streamers: err=%v calls=%d", err, p.Calls)
	}
	m = app.NewMonitorLives(p, []domain.Streamer{{Login: "a", AllowsClips: true}}, nil, nil, discardLog())
	p.SetStreams(domain.LiveStream{ID: "1", Login: "a"})
	if err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.SetStreams()
	if err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.Live()) != 0 {
		t.Fatal("expected no lives")
	}
}

func TestMonitorLivesGameFilter(t *testing.T) {
	ctx := context.Background()
	p := &fakes.Platform{}
	obs := &fakes.LiveObserver{}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	streamers := []domain.Streamer{
		{Login: "alice", AllowsClips: true, Layout: domain.LayoutBlur},
		{Login: "bob", AllowsClips: true, Layout: domain.LayoutBlur},
	}
	m := app.NewMonitorLives(p, streamers, domain.GameFilter{"Grand Theft Auto VI"}, obs, log)
	gta := func(id domain.StreamID, login string) domain.LiveStream {
		return domain.LiveStream{ID: id, Login: login, GameID: "1867279146", Game: "Grand Theft Auto VI"}
	}
	other := func(id domain.StreamID, login, game string) domain.LiveStream {
		return domain.LiveStream{ID: id, Login: login, GameID: "1", Game: game}
	}
	poll := func(streams ...domain.LiveStream) {
		t.Helper()
		p.SetStreams(streams...)
		if err := m.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	logins := func(ls []domain.LiveStream) string {
		var s []string
		for _, l := range ls {
			s = append(s, l.Login+":"+string(l.ID))
		}
		return strings.Join(s, ",")
	}

	// Alice plays GTA VI, bob is live in another game: only alice is monitored.
	poll(gta("s1", "alice"), other("s2", "bob", "Just Chatting"))
	started, ended := obs.Snapshot()
	if logins(started) != "alice:s1" || len(ended) != 0 {
		t.Fatalf("poll 1: started=%v ended=%v", started, ended)
	}
	if logins(m.Live()) != "alice:s1" || logins(m.OffGame()) != "bob:s2" {
		t.Fatalf("poll 1: live=%v offGame=%v", m.Live(), m.OffGame())
	}

	// Nothing changes: no notifications and the skipped live is not logged again.
	poll(gta("s1", "alice"), other("s2", "bob", "Just Chatting"))
	if n := strings.Count(logs.String(), "game outside the list"); n != 1 {
		t.Fatalf("off-game live logged %d times, want once:\n%s", n, logs.String())
	}

	// Bob starts playing GTA VI in the same broadcast; alice leaves for another game.
	poll(other("s1", "alice", "Minecraft"), gta("s2", "bob"))
	started, ended = obs.Snapshot()
	if logins(started) != "alice:s1,bob:s2" || logins(ended) != "alice:s1" {
		t.Fatalf("poll 3: started=%v ended=%v", started, ended)
	}
	if logins(m.Live()) != "bob:s2" || logins(m.OffGame()) != "alice:s1" {
		t.Fatalf("poll 3: live=%v offGame=%v", m.Live(), m.OffGame())
	}
	if !strings.Contains(logs.String(), "live left the monitored games") || strings.Count(logs.String(), "game outside the list") != 1 {
		t.Fatalf("leaving the list must be logged once as such:\n%s", logs.String())
	}

	// Alice comes back to GTA VI: the same broadcast is monitored again.
	poll(gta("s1", "alice"), gta("s2", "bob"))
	started, _ = obs.Snapshot()
	if logins(started) != "alice:s1,bob:s2,alice:s1" || len(m.OffGame()) != 0 {
		t.Fatalf("poll 4: started=%v offGame=%v", started, m.OffGame())
	}

	// Both go offline.
	poll()
	_, ended = obs.Snapshot()
	if logins(ended) != "alice:s1,alice:s1,bob:s2" || len(m.Live()) != 0 || len(m.OffGame()) != 0 {
		t.Fatalf("poll 5: ended=%v live=%v offGame=%v", ended, m.Live(), m.OffGame())
	}
}
