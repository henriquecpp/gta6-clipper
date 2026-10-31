package app_test

import (
	"context"
	"errors"
	"log/slog"
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
	m := app.NewMonitorLives(p, streamers, obs, discardLog())
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
	m := app.NewMonitorLives(p, nil, nil, discardLog())
	if err := m.Poll(context.Background()); err != nil || p.Calls != 0 {
		t.Fatalf("poll with no streamers: err=%v calls=%d", err, p.Calls)
	}
	m = app.NewMonitorLives(p, []domain.Streamer{{Login: "a", AllowsClips: true}}, nil, discardLog())
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
