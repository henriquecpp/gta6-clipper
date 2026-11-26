package capture

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// ErrStalled is returned by Capture when no audio arrived for the stall
// timeout. The source can hang without exiting: on 2026-11-25, after a
// network drop, a capture spent 9 h without audio and nothing restarted it.
var ErrStalled = errors.New("capture stalled: no audio")

// stallWatch stops a capture whose audio stopped flowing.
type stallWatch struct {
	timeout time.Duration
	now     func() time.Time
	last    atomic.Int64 // UnixNano of the last audio frame (or of the start)
	stalled atomic.Bool
}

func newStallWatch(timeout time.Duration, now func() time.Time) *stallWatch {
	w := &stallWatch{timeout: timeout, now: now}
	w.progress()
	return w
}

// progress records that audio arrived.
func (w *stallWatch) progress() { w.last.Store(w.now().UnixNano()) }

// run checks the audio a few times per timeout until ctx ends, and calls stop
// once (marking the capture as stalled) when it has been missing for the
// whole timeout.
func (w *stallWatch) run(ctx context.Context, stop func()) {
	t := time.NewTicker(max(w.timeout/4, time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.now().Sub(time.Unix(0, w.last.Load())) >= w.timeout {
				w.stalled.Store(true)
				stop()
				return
			}
		}
	}
}
