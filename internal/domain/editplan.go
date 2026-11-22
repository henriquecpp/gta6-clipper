package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Caption is one subtitle, with times relative to the start of the retained window.
type Caption struct {
	Start time.Duration
	End   time.Duration
	Text  string
}

// Post is the text delivered with the final video (never published automatically).
type Post struct {
	Headline string
	Text     string
}

// EditConstraints are the rules an edit plan must respect.
type EditConstraints struct {
	// WindowDuration is the length of the retained media.
	WindowDuration time.Duration
	// Peak is the peak position relative to the window start.
	Peak          time.Duration
	MinDuration   time.Duration
	MaxDuration   time.Duration
	PeakTolerance time.Duration
	// HeadlineMaxChars limits the headline length (in characters).
	HeadlineMaxChars int
	// Streamer must be credited in the post text.
	Streamer string
}

// EditRetry carries the previous invalid answer back to the editor once.
type EditRetry struct {
	Previous EditProposal
	Problem  string
}

// EditRequest is everything the editor sees. The model does not see the
// video: it decides from the speech and the signals.
type EditRequest struct {
	ClipID      ClipID
	Streamer    string
	Game        string
	StreamTitle string
	// Language is the detected spoken language; captions are in OutputLanguage.
	Language       string
	OutputLanguage string
	Transcript     Transcript
	// Signals has one point per second, Pos relative to the window start.
	Signals []SignalPoint
	// ViewerClipTitles are the titles viewers gave to their Twitch clips of
	// the moment (hints for the headline; may be jokes or unrelated).
	ViewerClipTitles []string
	// GameTranscript is the same audio transcribed in the game's language
	// (English) when the streamer speaks another one: game characters read
	// correctly there and garbled in Transcript. Empty when not available.
	GameTranscript Transcript
	Constraints    EditConstraints
	Retry          *EditRetry
}

// ProposedCaption is a caption as proposed by the editor (seconds).
type ProposedCaption struct {
	StartS float64
	EndS   float64
	Text   string
}

// EditProposal is the raw, unvalidated answer of the editor.
type EditProposal struct {
	StartS    float64
	EndS      float64
	Headline  string
	Captions  []ProposedCaption
	PostText  string
	Reasoning string
	// YouTubeTitle and SearchTags are the search metadata of the Short.
	YouTubeTitle string
	SearchTags   []string
	// ToolUseID and RawInput keep the model's tool call for the retry
	// conversation; RawText is any plain text the model returned instead.
	ToolUseID string
	RawInput  string
	RawText   string
}

// EditPlan is a validated edit: the cut, headline, captions and post text,
// plus the YouTube title and the search tags of the moment.
type EditPlan struct {
	Start     time.Duration
	End       time.Duration
	Headline  string
	Captions  []Caption
	PostText  string
	Reasoning string
	// YouTubeTitle is the search-oriented title of the Short (empty: use the
	// headline). SearchTags are phrases viewers would type to find the moment.
	YouTubeTitle string
	SearchTags   []string
}

// Limits of the YouTube metadata written by the editor. The title leaves
// room for the title hashtags within YouTube's 100 characters.
const (
	MaxYouTubeTitleChars = 85
	MaxSearchTags        = 12
	MaxSearchTagChars    = 40
)

// Duration is the length of the cut.
func (p EditPlan) Duration() time.Duration { return p.End - p.Start }

func secsToDur(s float64) (time.Duration, bool) {
	if math.IsNaN(s) || math.IsInf(s, 0) || s < -1e6 || s > 1e6 {
		return 0, false
	}
	return time.Duration(math.Round(s*1000)) * time.Millisecond, true
}

// NewEditPlan validates a proposal against the constraints (CLAUDE.md §12).
// Every problem is reported, wrapped in ErrInvalidEditPlan, so the editor can
// fix them all in its single retry.
func NewEditPlan(p EditProposal, c EditConstraints) (EditPlan, error) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	start, ok1 := secsToDur(p.StartS)
	end, ok2 := secsToDur(p.EndS)
	if !ok1 || !ok2 {
		add("start_s/end_s must be finite numbers")
	}
	if start < 0 || start >= end || end > c.WindowDuration {
		add("need 0 <= start_s < end_s <= %.1f (window duration); got start_s=%.2f end_s=%.2f", c.WindowDuration.Seconds(), p.StartS, p.EndS)
	}
	if d := end - start; d < c.MinDuration || d > c.MaxDuration {
		add("cut duration %.1fs outside [%.0fs, %.0fs]", d.Seconds(), c.MinDuration.Seconds(), c.MaxDuration.Seconds())
	}
	if c.Peak < start-c.PeakTolerance || c.Peak > end+c.PeakTolerance {
		add("the cut must contain the peak at %.1fs (tolerance %.0fs)", c.Peak.Seconds(), c.PeakTolerance.Seconds())
	}
	headline := strings.TrimSpace(p.Headline)
	if headline == "" {
		add("headline is empty")
	} else if n := utf8.RuneCountInString(headline); c.HeadlineMaxChars > 0 && n > c.HeadlineMaxChars {
		add("headline has %d characters (max %d)", n, c.HeadlineMaxChars)
	}
	post := strings.TrimSpace(p.PostText)
	if post == "" {
		add("post_text is empty")
	} else if c.Streamer != "" && !strings.Contains(strings.ToLower(post), strings.ToLower(c.Streamer)) {
		add("post_text must credit the streamer %q", c.Streamer)
	}

	captions := make([]Caption, 0, len(p.Captions))
	prevEnd := time.Duration(math.MinInt64)
	for i, pc := range p.Captions {
		cs, okS := secsToDur(pc.StartS)
		ce, okE := secsToDur(pc.EndS)
		text := strings.TrimSpace(pc.Text)
		switch {
		case !okS || !okE || cs >= ce:
			add("caption %d: need start_s < end_s", i)
		case text == "":
			add("caption %d: empty text", i)
		case cs < start || ce > end:
			add("caption %d (%.2f–%.2f) is outside the cut (%.2f–%.2f)", i, pc.StartS, pc.EndS, p.StartS, p.EndS)
		case cs < prevEnd:
			add("caption %d starts at %.2f before the previous one ends (captions must be ordered and not overlap)", i, pc.StartS)
		}
		if okE {
			prevEnd = max(prevEnd, ce)
		}
		captions = append(captions, Caption{Start: cs, End: ce, Text: text})
	}
	if len(p.Captions) == 0 {
		add("captions are empty")
	}
	if len(problems) > 0 {
		return EditPlan{}, fmt.Errorf("%w: %s", ErrInvalidEditPlan, strings.Join(problems, "; "))
	}
	return EditPlan{Start: start, End: end, Headline: headline, Captions: captions, PostText: post, Reasoning: strings.TrimSpace(p.Reasoning),
		YouTubeTitle: youtubeTitle(p.YouTubeTitle), SearchTags: searchTags(p.SearchTags)}, nil
}

// youtubeTitle cleans the YouTube title. It is optional metadata: a bad one
// falls back to the headline instead of failing the edit (and its retry).
func youtubeTitle(s string) string {
	s = strings.Join(strings.Fields(stripAngles(s)), " ")
	if utf8.RuneCountInString(s) > MaxYouTubeTitleChars {
		return ""
	}
	return s
}

// searchTags keeps up to MaxSearchTags distinct tags without hashtags,
// commas, quotes or angle brackets; overly long ones are dropped.
func searchTags(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
			switch r {
			case '<', '>', ',', '"', '#':
				return -1
			}
			return r
		}, t)), " ")
		key := strings.ToLower(t)
		if t == "" || seen[key] || utf8.RuneCountInString(t) > MaxSearchTagChars {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) == MaxSearchTags {
			break
		}
	}
	return out
}

// IsInvalidEditPlan reports whether err is a validation error of the plan.
func IsInvalidEditPlan(err error) bool { return errors.Is(err, ErrInvalidEditPlan) }

// SnapToWords moves the cut boundaries so no word is cut: a boundary inside a
// word goes to the nearest edge of that word, then the start gets margin
// earlier and the end margin later (without entering the neighbour words).
// If the adjusted plan breaks the constraints, the original is returned.
// Captions are clipped to the new cut.
func (p EditPlan) SnapToWords(t Transcript, margin time.Duration, c EditConstraints) EditPlan {
	words := t.Words
	start, end := p.Start, p.End
	for _, w := range words {
		if w.Start < start && start < w.End {
			if start-w.Start <= w.End-start {
				start = w.Start
			} else {
				start = w.End
			}
		}
		if w.Start < end && end < w.End {
			if end-w.Start < w.End-end {
				end = w.Start
			} else {
				end = w.End
			}
		}
	}
	// Margins inside the silence around the boundary.
	prevEnd, nextStart := time.Duration(0), c.WindowDuration
	for _, w := range words {
		if w.End <= start && w.End > prevEnd {
			prevEnd = w.End
		}
		if w.Start >= end && w.Start < nextStart {
			nextStart = w.Start
		}
	}
	start = max(start-margin, prevEnd, 0)
	end = min(end+margin, nextStart, c.WindowDuration)

	out := p
	out.Start, out.End = start, end
	d := end - start
	if start >= end || d < c.MinDuration || d > c.MaxDuration || c.Peak < start-c.PeakTolerance || c.Peak > end+c.PeakTolerance {
		return p
	}
	out.Captions = nil
	for _, cap := range p.Captions {
		cap.Start, cap.End = max(cap.Start, start), min(cap.End, end)
		if cap.Start < cap.End {
			out.Captions = append(out.Captions, cap)
		}
	}
	return out
}
