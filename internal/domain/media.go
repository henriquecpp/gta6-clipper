package domain

import "time"

// PreviewSpec describes the low-resolution preview cut from a retained file.
// Start and Duration are relative to the start of the retained media.
type PreviewSpec struct {
	Start    time.Duration
	Duration time.Duration
	Height   int
	MaxBytes int64
}

// PreviewWindow places a preview of the given duration around the peak (two
// thirds before it, one third after), inside the retained window.
func PreviewWindow(w Window, peak StreamPos, duration time.Duration) (start, length time.Duration) {
	total := w.Duration()
	if duration <= 0 || duration >= total {
		return 0, total
	}
	rel := time.Duration(peak - w.From)
	start = rel - duration*2/3
	start = max(0, min(start, total-duration))
	return start, duration
}

// RenderSpec describes the final vertical video. Times are relative to the
// start of the retained media.
type RenderSpec struct {
	ClipID   ClipID
	Streamer string
	Start    time.Duration
	End      time.Duration
	Width    int
	Height   int
	FPS      int
	Layout   Layout
	Stack    StackRegions
	Headline string
	Captions []Caption
	// Words are the transcribed words, used to time the karaoke highlight.
	Words    []Word
	MaxBytes int64
}
