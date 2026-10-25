package domain

import (
	"strings"
	"time"
)

// Word is a transcribed word with its time range relative to the start of the
// transcribed media (the retained window).
type Word struct {
	Text  string
	Start time.Duration
	End   time.Duration
	Prob  float64
}

// Transcript is the result of transcribing a retained window.
type Transcript struct {
	// Language is the detected ISO 639-1 code (e.g. "en", "pt").
	Language string
	Words    []Word
}

// Text joins all words with single spaces.
func (t Transcript) Text() string {
	parts := make([]string, 0, len(t.Words))
	for _, w := range t.Words {
		if s := strings.TrimSpace(w.Text); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// Between returns the words that overlap [from, to).
func (t Transcript) Between(from, to time.Duration) []Word {
	var out []Word
	for _, w := range t.Words {
		if w.End > from && w.Start < to {
			out = append(out, w)
		}
	}
	return out
}
