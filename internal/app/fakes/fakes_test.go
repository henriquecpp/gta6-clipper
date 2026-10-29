package fakes_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

var testTime = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func TestClock(t *testing.T) {
	c := fakes.NewClock(testTime)
	c.Advance(time.Minute)
	if !c.Now().Equal(testTime.Add(time.Minute)) {
		t.Fatal("Advance")
	}
	c.Set(testTime)
	if !c.Now().Equal(testTime) {
		t.Fatal("Set")
	}
}

func TestPlatform(t *testing.T) {
	p := &fakes.Platform{Clips: map[string][]domain.ViewerClip{
		"b1": {{ID: "old", CreatedAt: testTime.Add(-time.Hour)}, {ID: "new", CreatedAt: testTime}},
	}}
	p.SetStreams(domain.LiveStream{Login: "a"}, domain.LiveStream{Login: "b"})
	got, err := p.LiveStreams(context.Background(), []string{"a"})
	if err != nil || len(got) != 1 || got[0].Login != "a" {
		t.Fatalf("LiveStreams = %v, %v", got, err)
	}
	boom := errors.New("boom")
	p.SetStreamErr(boom)
	if _, err := p.LiveStreams(context.Background(), []string{"a"}); !errors.Is(err, boom) {
		t.Fatal("expected error")
	}
	clips, err := p.RecentClips(context.Background(), "b1", testTime.Add(-time.Minute))
	if err != nil || len(clips) != 1 || clips[0].ID != "new" {
		t.Fatalf("RecentClips = %v, %v", clips, err)
	}
	p.ClipsErr = boom
	if _, err := p.RecentClips(context.Background(), "b1", testTime); !errors.Is(err, boom) {
		t.Fatal("expected clips error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.ClipsErr = nil
	if _, err := p.RecentClips(ctx, "b1", testTime); err == nil {
		t.Fatal("expected ctx error")
	}
	if _, err := p.LiveStreams(ctx, nil); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestLiveObserver(t *testing.T) {
	var o fakes.LiveObserver
	o.StreamStarted(context.Background(), domain.Streamer{}, domain.LiveStream{ID: "1"})
	o.StreamEnded(context.Background(), domain.Streamer{}, domain.LiveStream{ID: "2"})
	s, e := o.Snapshot()
	if len(s) != 1 || len(e) != 1 || s[0].ID != "1" || e[0].ID != "2" {
		t.Fatalf("snapshot %v %v", s, e)
	}
}
