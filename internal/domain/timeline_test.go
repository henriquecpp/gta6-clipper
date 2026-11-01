package domain

import (
	"testing"
	"time"
)

func TestTimelineAlignment(t *testing.T) {
	capStart := time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	tl := Timeline{
		CaptureStart: capStart,
		StreamStart:  capStart.Add(-2 * time.Hour),
		Offsets:      Offsets{CaptureLatency: 4 * time.Second, Chat: 8 * time.Second, Clip: 5 * time.Second},
	}
	// Event at capStart+60s is received at +64s → position 64s.
	if p := tl.EventPos(capStart.Add(time.Minute)); p != StreamPos(64*time.Second) {
		t.Fatalf("EventPos = %v", p)
	}
	if w := tl.WallTime(StreamPos(64 * time.Second)); !w.Equal(capStart.Add(time.Minute)) {
		t.Fatalf("WallTime = %v", w)
	}
	if p := tl.CapturePos(capStart.Add(90 * time.Second)); p != StreamPos(90*time.Second) {
		t.Fatalf("CapturePos = %v", p)
	}
	// Chat received at +70s reacts to the event at +62s → position 66s.
	if p := tl.ChatPos(capStart.Add(70 * time.Second)); p != StreamPos(66*time.Second) {
		t.Fatalf("ChatPos = %v", p)
	}
	// Clip with VOD offset: stream started 2h before capture; clip at VOD
	// 2h+30s, 20 s long → event at capStart+40s → position 44s.
	vod := ViewerClip{Duration: 20 * time.Second, VODOffset: 2*time.Hour + 30*time.Second, HasVODOffset: true}
	if p := tl.ClipPos(vod); p != StreamPos(44*time.Second) {
		t.Fatalf("ClipPos(vod) = %v", p)
	}
	// Without VOD offset: created at +100s, 30 s long → event at
	// 100 − 5 − 15 = +80s → position 84s.
	noVOD := ViewerClip{Duration: 30 * time.Second, CreatedAt: capStart.Add(100 * time.Second)}
	if p := tl.ClipPos(noVOD); p != StreamPos(84*time.Second) {
		t.Fatalf("ClipPos(created) = %v", p)
	}
	// Unknown stream start falls back to created_at.
	tl.StreamStart = time.Time{}
	vod.CreatedAt = capStart.Add(100 * time.Second)
	if p := tl.ClipPos(vod); p != StreamPos(89*time.Second) {
		t.Fatalf("ClipPos fallback = %v", p)
	}
}

func TestRetentionWindow(t *testing.T) {
	w, ok := RetentionWindow(StreamPos(90*time.Second), 60*time.Second, 30*time.Second)
	if !ok || w.From != StreamPos(30*time.Second) || w.To != StreamPos(120*time.Second) {
		t.Fatalf("window = %+v %v", w, ok)
	}
	// Clamped at the capture start.
	w, ok = RetentionWindow(StreamPos(10*time.Second), 60*time.Second, 30*time.Second)
	if !ok || w.From != 0 || w.To != StreamPos(40*time.Second) {
		t.Fatalf("clamped window = %+v", w)
	}
	for _, bad := range []struct {
		peak      StreamPos
		pre, post time.Duration
	}{
		{StreamPos(-5 * time.Second), time.Minute, time.Second},
		{0, time.Minute, 0},
		{StreamPos(time.Minute), -time.Second, time.Second},
		{StreamPos(time.Minute), time.Second, -time.Second},
	} {
		if _, ok := RetentionWindow(bad.peak, bad.pre, bad.post); ok {
			t.Errorf("RetentionWindow(%v, %v, %v) should fail", bad.peak, bad.pre, bad.post)
		}
	}
}

// FuzzRetentionWindow: alignment never yields a window ending before it
// starts, and an accepted window always contains the peak (CLAUDE.md §17).
func FuzzRetentionWindow(f *testing.F) {
	f.Add(int64(90e9), int64(60e9), int64(30e9))
	f.Add(int64(-1), int64(0), int64(1))
	f.Add(int64(0), int64(0), int64(0))
	f.Fuzz(func(t *testing.T, peak, pre, post int64) {
		w, ok := RetentionWindow(StreamPos(peak), time.Duration(pre), time.Duration(post))
		if !ok {
			return
		}
		if w.To <= w.From || w.From < 0 || !w.Contains(StreamPos(peak)) {
			t.Fatalf("invalid window %+v for peak=%d pre=%d post=%d", w, peak, pre, post)
		}
	})
}
