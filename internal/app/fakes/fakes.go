package fakes

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Clock is a manually driven clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

var _ app.Clock = (*Clock)(nil)

// NewClock returns a clock set to t.
func NewClock(t time.Time) *Clock { return &Clock{now: t.UTC()} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}

// Platform is a scripted Platform.
type Platform struct {
	mu        sync.Mutex
	Streams   []domain.LiveStream
	StreamErr error
	Clips     map[string][]domain.ViewerClip
	ClipsErr  error
	Calls     int
}

var _ app.Platform = (*Platform)(nil)

// SetStreams replaces the live streams returned by LiveStreams.
func (p *Platform) SetStreams(s ...domain.LiveStream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Streams = s
}

// SetStreamErr makes LiveStreams fail with err (nil clears it).
func (p *Platform) SetStreamErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.StreamErr = err
}

// LiveStreams returns the scripted streams whose login is in logins.
func (p *Platform) LiveStreams(ctx context.Context, logins []string) ([]domain.LiveStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.StreamErr != nil {
		return nil, p.StreamErr
	}
	want := make(map[string]bool, len(logins))
	for _, l := range logins {
		want[strings.ToLower(l)] = true
	}
	var out []domain.LiveStream
	for _, s := range p.Streams {
		if want[strings.ToLower(s.Login)] {
			out = append(out, s)
		}
	}
	return out, nil
}

// RecentClips returns the scripted clips of the broadcaster created at or after since.
func (p *Platform) RecentClips(ctx context.Context, broadcasterID string, since time.Time) ([]domain.ViewerClip, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ClipsErr != nil {
		return nil, p.ClipsErr
	}
	var out []domain.ViewerClip
	for _, c := range p.Clips[broadcasterID] {
		if !c.CreatedAt.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

// LiveObserver records the notifications it receives.
type LiveObserver struct {
	mu      sync.Mutex
	Started []domain.LiveStream
	Ended   []domain.LiveStream
}

var _ app.LiveObserver = (*LiveObserver)(nil)

// StreamStarted records a start.
func (o *LiveObserver) StreamStarted(_ context.Context, _ domain.Streamer, l domain.LiveStream) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Started = append(o.Started, l)
}

// StreamEnded records an end.
func (o *LiveObserver) StreamEnded(_ context.Context, _ domain.Streamer, l domain.LiveStream) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Ended = append(o.Ended, l)
}

// Snapshot returns copies of the recorded starts and ends.
func (o *LiveObserver) Snapshot() (started, ended []domain.LiveStream) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]domain.LiveStream(nil), o.Started...), append([]domain.LiveStream(nil), o.Ended...)
}
