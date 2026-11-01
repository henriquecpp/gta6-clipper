package domain

import (
	"fmt"
	"time"
)

// StreamPos is a position in the captured stream: the time elapsed since the
// capture started. It is the single time reference for every signal.
type StreamPos time.Duration

// Duration converts the position to a time.Duration.
func (p StreamPos) Duration() time.Duration { return time.Duration(p) }

// String formats the position as HH:MM:SS.
func (p StreamPos) String() string {
	d := time.Duration(p)
	neg := d < 0
	if neg {
		d = -d
	}
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	if neg {
		return fmt.Sprintf("-%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// Window is a closed range of the captured stream.
type Window struct {
	From StreamPos
	To   StreamPos
}

// Duration is the length of the window.
func (w Window) Duration() time.Duration { return time.Duration(w.To - w.From) }

// Contains reports whether p lies in the window.
func (w Window) Contains(p StreamPos) bool { return p >= w.From && p <= w.To }

// Valid reports whether the window is non-empty and starts at or after zero.
func (w Window) Valid() bool { return w.From >= 0 && w.To > w.From }

// Timeline aligns the clocks of every signal source with the captured stream
// (CLAUDE.md §9). StreamPos p is the content received at wall time
// CaptureStart + p; an event that happened at wall time e (at the streamer)
// is received CaptureLatency later.
type Timeline struct {
	// CaptureStart is the wall time at which StreamPos 0 was received.
	CaptureStart time.Time
	// StreamStart is when the broadcast started (origin of VOD offsets).
	StreamStart time.Time
	Offsets     Offsets
}

// EventPos maps the wall time of an event to its position in the capture.
func (t Timeline) EventPos(at time.Time) StreamPos {
	return StreamPos(at.Sub(t.CaptureStart) + t.Offsets.CaptureLatency)
}

// WallTime is the inverse of EventPos: when the event at p happened.
func (t Timeline) WallTime(p StreamPos) time.Time {
	return t.CaptureStart.Add(time.Duration(p) - t.Offsets.CaptureLatency)
}

// CapturePos is the position of the content being received at wall time now.
func (t Timeline) CapturePos(now time.Time) StreamPos {
	return StreamPos(now.Sub(t.CaptureStart))
}

// ChatPos maps a chat message received at wall time m to the event it reacts to.
func (t Timeline) ChatPos(m time.Time) StreamPos {
	return t.EventPos(m.Add(-t.Offsets.Chat))
}

// ClipPos estimates the event of a viewer clip as the middle of the clip:
// from its VOD offset when known, else from its creation time minus the
// viewer delay (Offsets.Clip).
func (t Timeline) ClipPos(c ViewerClip) StreamPos {
	half := c.Duration / 2
	if c.HasVODOffset && !t.StreamStart.IsZero() {
		return t.EventPos(t.StreamStart.Add(c.VODOffset + half))
	}
	return t.EventPos(c.CreatedAt.Add(-t.Offsets.Clip - half))
}

// RetentionWindow is [peak − pre, peak + post] clamped at the start of the
// capture. ok is false when no valid (non-empty, peak-containing) window exists.
func RetentionWindow(peak StreamPos, pre, post time.Duration) (w Window, ok bool) {
	if pre < 0 || post < 0 {
		return Window{}, false
	}
	from := max(StreamPos(0), peak-StreamPos(pre))
	to := peak + StreamPos(post)
	w = Window{From: from, To: to}
	if !w.Valid() || !w.Contains(peak) {
		return Window{}, false
	}
	return w, true
}
