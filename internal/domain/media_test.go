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
		{160 * time.Second, 30 * time.Second, 48 * time.Second, 30 * time.Second}, // 60 s in, 12 s before, 18 s after
		{105 * time.Second, 30 * time.Second, 0, 30 * time.Second},                // clamped at start
		{189 * time.Second, 30 * time.Second, 60 * time.Second, 30 * time.Second}, // clamped at end
		{150 * time.Second, 2 * time.Minute, 0, 90 * time.Second},                 // longer than media
		{150 * time.Second, 0, 0, 90 * time.Second},
	}
	for _, c := range cases {
		s, l := PreviewWindow(w, StreamPos(c.peak), c.dur, 12*time.Second)
		if s != c.start || l != c.want {
			t.Errorf("peak %v dur %v: got %v+%v, want %v+%v", c.peak, c.dur, s, l, c.start, c.want)
		}
	}
	// before is clamped to [0, duration].
	if s, _ := PreviewWindow(w, StreamPos(160*time.Second), 30*time.Second, -time.Second); s != 60*time.Second {
		t.Errorf("negative before: start %v, want the peak (60s)", s)
	}
	if s, _ := PreviewWindow(w, StreamPos(160*time.Second), 30*time.Second, time.Hour); s != 30*time.Second {
		t.Errorf("before > duration: start %v, want 30s (preview ends at the peak)", s)
	}
}
