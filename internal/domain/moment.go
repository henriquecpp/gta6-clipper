package domain

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// SignalKind identifies a detection signal.
type SignalKind string

// Signal kinds (CLAUDE.md §9).
const (
	SignalVoice SignalKind = "voice"
	SignalChat  SignalKind = "chat"
	SignalClips SignalKind = "clips"
)

// SignalKinds lists every kind in a stable order.
var SignalKinds = []SignalKind{SignalVoice, SignalChat, SignalClips}

// Signal is one sample of a signal on the stream timeline. Value is the
// normalized intensity (for voice and chat, a z-score; for clips, a count).
type Signal struct {
	Kind  SignalKind
	Pos   StreamPos
	Value float64
}

// AudioFrame is the energy of a short audio frame (20–50 ms) of the capture,
// already classified by the voice activity detector.
type AudioFrame struct {
	Pos StreamPos
	// DBFS is the RMS energy in dB relative to full scale (<= 0).
	DBFS float64
	// Speech is true when the VAD classified the frame as voice.
	Speech bool
}

// SignalPoint is the per-second state of all signals, used to build the
// timeline sent to the editor.
type SignalPoint struct {
	Pos   StreamPos
	Voice float64
	Chat  float64
	Clips float64
}

// Moment is a detected peak with the signal values that produced it.
type Moment struct {
	Peak  StreamPos
	Score float64
	// Values holds every signal's (clamped) value at the peak.
	Values map[SignalKind]float64
	// ViewerClips is how many viewer clips were grouped around the peak.
	ViewerClips int
	// Late is true when the moment came only from the late clip signal.
	Late bool
	// ClipsAt is where the viewer clips placed a late moment (its Peak is
	// the reaction found before it); later clips of the same cluster are
	// matched against it. Not persisted.
	ClipsAt StreamPos
	// ClipTitles are the titles viewers gave to their Twitch clips of this
	// moment (most viewed first, at most MaxClipTitles). They are hints for
	// the editor's headline, not facts.
	ClipTitles []string
}

// MaxClipTitles bounds the viewer clip titles kept per moment.
const MaxClipTitles = 5

// maxClipTitleRunes bounds one title (Twitch does not document a limit).
const maxClipTitleRunes = 100

// CleanClipTitle normalizes a viewer clip title and returns "" when it says
// nothing about the moment: empty, or equal to the stream title, which Twitch
// uses as the default title of every clip.
func CleanClipTitle(title, streamTitle string) string {
	t := strings.Join(strings.Fields(title), " ")
	if t == "" || strings.EqualFold(t, strings.Join(strings.Fields(streamTitle), " ")) {
		return ""
	}
	if utf8.RuneCountInString(t) > maxClipTitleRunes {
		t = strings.TrimSpace(string([]rune(t)[:maxClipTitleRunes])) + "…"
	}
	return t
}

// MergeClipTitles appends the new titles that are not there yet (ignoring
// case), keeping at most MaxClipTitles. Empty titles are skipped.
func MergeClipTitles(titles []string, add ...string) []string {
	out := titles
	for _, t := range add {
		if len(out) >= MaxClipTitles {
			break
		}
		if t == "" || containsFold(out, t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// Clone returns a deep copy.
func (m Moment) Clone() Moment {
	c := m
	c.ClipTitles = slices.Clone(m.ClipTitles)
	if m.Values != nil {
		c.Values = make(map[SignalKind]float64, len(m.Values))
		for k, v := range m.Values {
			c.Values[k] = v
		}
	}
	return c
}
