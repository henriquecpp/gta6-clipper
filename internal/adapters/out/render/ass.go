package render

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ASSStyle configures the burned-in subtitles.
type ASSStyle struct {
	Font string
	// MaxLineChars is the maximum characters per caption line.
	MaxLineChars int
	// HeadlineLineChars is the maximum characters per headline line.
	HeadlineLineChars int
}

// Safe area for TikTok/Reels/Shorts: captions stay above the bottom 25% and
// away from the right-hand buttons (CLAUDE.md §10).
const (
	safeBottom   = 0.25
	safeRight    = 0.12
	safeLeft     = 0.055
	headlineTop  = 0.08
	avgCharWidth = 0.56 // average glyph width / font size for bold sans
)

// assTime formats a duration as H:MM:SS.cc.
func assTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	cs := int64(math.Round(float64(d) / float64(10*time.Millisecond)))
	return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, cs/6000%60, cs/100%60, cs%100)
}

// assText neutralizes ASS override characters in user text.
func assText(s string) string {
	r := strings.NewReplacer("{", "(", "}", ")", `\`, "/", "\n", " ", "\r", " ")
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// wrapWords breaks words into lines of at most maxChars characters, preferring
// to break after punctuation once a line is at least half full.
func wrapWords(words []string, maxChars int) [][]string {
	var lines [][]string
	var cur []string
	n := 0
	for _, w := range words {
		wl := utf8.RuneCountInString(w)
		if len(cur) > 0 && n+1+wl > maxChars {
			// Do not end a line with a short function word ("da", "e", "de"):
			// it reads better at the start of the next line.
			var carry []string
			if last := cur[len(cur)-1]; len(cur) > 1 && utf8.RuneCountInString(last) <= 2 && isWord(last) {
				carry, cur = []string{last}, cur[:len(cur)-1]
			}
			lines = append(lines, cur)
			cur, n = carry, 0
			if len(carry) > 0 {
				n = utf8.RuneCountInString(carry[0])
			}
		}
		if len(cur) > 0 {
			n++
		}
		cur = append(cur, w)
		n += wl
		if last, _ := utf8.DecodeLastRuneInString(w); unicode.IsPunct(last) && n >= maxChars/2 {
			lines = append(lines, cur)
			cur, n = nil, 0
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

func isWord(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// karaoke returns the highlight duration of each caption word. When the
// caption matches the transcribed words one to one (same language), their
// start times are used; otherwise (translation) the caption time is split in
// proportion to the word lengths.
func karaoke(words []string, c domain.Caption, spoken []domain.Word) []time.Duration {
	total := c.End - c.Start
	out := make([]time.Duration, len(words))
	if len(words) == 0 || total <= 0 {
		return out
	}
	var inCap []domain.Word
	for _, w := range spoken {
		if w.Start >= c.Start-200*time.Millisecond && w.Start < c.End {
			inCap = append(inCap, w)
		}
	}
	if len(inCap) == len(words) {
		for i := range words {
			start := max(inCap[i].Start, c.Start)
			end := c.End
			if i+1 < len(words) {
				end = max(inCap[i+1].Start, start)
			}
			out[i] = end - start
		}
		out[0] += max(inCap[0].Start-c.Start, 0) // lead-in silence belongs to the first word
		return out
	}
	totalRunes := 0
	for _, w := range words {
		totalRunes += max(utf8.RuneCountInString(w), 1)
	}
	var used time.Duration
	for i, w := range words {
		if i == len(words)-1 {
			out[i] = total - used
			break
		}
		out[i] = total * time.Duration(max(utf8.RuneCountInString(w), 1)) / time.Duration(totalRunes)
		used += out[i]
	}
	return out
}

func centis(d time.Duration) int64 {
	return int64(math.Round(float64(d) / float64(10*time.Millisecond)))
}

// captionFontSize fits MaxLineChars characters in the safe width.
func captionFontSize(width, maxChars int) int {
	usable := float64(width) * (1 - safeLeft - safeRight)
	return int(math.Min(76, usable/(avgCharWidth*float64(max(maxChars, 1)))))
}

// BuildASS renders the subtitle file of a final video: the headline at the
// top for the whole clip and word-by-word karaoke captions in the safe area.
// Times in the output are relative to the start of the cut.
func BuildASS(spec domain.RenderSpec, style ASSStyle) string {
	w, h := spec.Width, spec.Height
	if style.MaxLineChars <= 0 {
		style.MaxLineChars = 28
	}
	if style.HeadlineLineChars <= 0 {
		style.HeadlineLineChars = 24
	}
	if style.Font == "" {
		style.Font = "Noto Sans"
	}
	capSize := captionFontSize(w, style.MaxLineChars)
	headSize := int(math.Round(float64(capSize) * 0.9))
	marginL := int(math.Round(float64(w) * safeLeft))
	marginR := int(math.Round(float64(w) * safeRight))
	marginV := int(math.Round(float64(h) * safeBottom))
	headV := int(math.Round(float64(h) * headlineTop))

	var b strings.Builder
	fmt.Fprintf(&b, "[Script Info]\nScriptType: v4.00+\nPlayResX: %d\nPlayResY: %d\nWrapStyle: 2\nScaledBorderAndShadow: yes\nYCbCr Matrix: TV.709\n\n", w, h)
	b.WriteString("[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	// Karaoke: words switch from SecondaryColour (white) to PrimaryColour (yellow) as they are spoken.
	fmt.Fprintf(&b, "Style: Caption,%s,%d,&H0000E5FF,&H00FFFFFF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,5,2,2,%d,%d,%d,1\n", style.Font, capSize, marginL, marginR, marginV)
	fmt.Fprintf(&b, "Style: Headline,%s,%d,&H00FFFFFF,&H00FFFFFF,&H64000000,&H00000000,-1,0,0,0,100,100,0,0,3,16,0,8,%d,%d,%d,1\n\n", style.Font, headSize, marginL, marginR, headV)
	b.WriteString("[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")

	dur := spec.End - spec.Start
	if head := assText(spec.Headline); head != "" {
		lines := wrapWords(strings.Fields(head), style.HeadlineLineChars)
		parts := make([]string, len(lines))
		for i, l := range lines {
			parts[i] = strings.Join(l, " ")
		}
		fmt.Fprintf(&b, "Dialogue: 1,%s,%s,Headline,,0,0,0,,%s\n", assTime(0), assTime(dur), strings.Join(parts, `\N`))
	}

	for _, c := range spec.Captions {
		c.Start, c.End = max(c.Start, spec.Start), min(c.End, spec.End)
		if c.End <= c.Start {
			continue
		}
		words := strings.Fields(assText(c.Text))
		if len(words) == 0 {
			continue
		}
		durs := karaoke(words, c, spec.Words)
		lines := wrapWords(words, style.MaxLineChars)
		// At most two lines per event: longer captions become consecutive events.
		wi := 0
		evStart := c.Start
		for li := 0; li < len(lines); li += 2 {
			group := lines[li:min(li+2, len(lines))]
			var text strings.Builder
			var evDur time.Duration
			for gi, line := range group {
				if gi > 0 {
					text.WriteString(`\N`)
				}
				for k, word := range line {
					if k > 0 {
						text.WriteByte(' ')
					}
					fmt.Fprintf(&text, `{\k%d}%s`, centis(durs[wi]), word)
					evDur += durs[wi]
					wi++
				}
			}
			evEnd := evStart + evDur
			if li+2 >= len(lines) {
				evEnd = c.End
			}
			fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Caption,,0,0,0,,%s\n", assTime(evStart-spec.Start), assTime(evEnd-spec.Start), text.String())
			evStart = evEnd
		}
	}
	return b.String()
}
