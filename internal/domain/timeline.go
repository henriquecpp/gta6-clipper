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
