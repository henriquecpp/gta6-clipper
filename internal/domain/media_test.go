package domain

import (
	"testing"
	"time"
)

func TestPreviewWindow(t *testing.T) {
	w := Window{From: StreamPos(100 * time.Second), To: StreamPos(190 * time.Second)} // 90 s
	cases := []struct {
		peak        time.Duration
		dur         time.Duration
		start, want time.Duration
	}{
		{160 * time.Second, 30 * time.Second, 40 * time.Second, 30 * time.Second}, // 60 s in, 20 s before
		{105 * time.Second, 30 * time.Second, 0, 30 * time.Second},                // clamped at start
		{189 * time.Second, 30 * time.Second, 60 * time.Second, 30 * time.Second}, // clamped at end
		{150 * time.Second, 2 * time.Minute, 0, 90 * time.Second},                 // longer than media
		{150 * time.Second, 0, 0, 90 * time.Second},
	}
	for _, c := range cases {
		s, l := PreviewWindow(w, StreamPos(c.peak), c.dur)
		if s != c.start || l != c.want {
			t.Errorf("peak %v dur %v: got %v+%v, want %v+%v", c.peak, c.dur, s, l, c.start, c.want)
		}
	}
}
