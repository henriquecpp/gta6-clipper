// Package whisper implements the Transcriber port against the whisper.cpp HTTP
// server (whisper-server) running in the `whisper` container.
package whisper

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// TimestampSource selects which whisper.cpp timing is used for word starts.
type TimestampSource string

const (
	// TimestampsToken uses the token t0/t1 timestamps.
	TimestampsToken TimestampSource = "token"
	// TimestampsDTW uses the DTW-aligned token time (t_dtw) for word starts
	// when available (requires the server to run with --dtw).
	TimestampsDTW TimestampSource = "dtw"
)

// verboseJSON mirrors the response of whisper-server for
// response_format=verbose_json (examples/server/server.cpp, tag v1.9.5).
// Note: despite the name, segments[].words[] holds tokens, not words.
type verboseJSON struct {
	Language              string             `json:"language"`
	DetectedLanguage      string             `json:"detected_language"`
	LanguageProbabilities map[string]float64 `json:"language_probabilities"`
	Duration              float64            `json:"duration"`
	Text                  string             `json:"text"`
	Segments              []struct {
		Text  string  `json:"text"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Words []struct {
			Word        string   `json:"word"`
			Start       *float64 `json:"start"`
			End         *float64 `json:"end"`
			TDTW        *int64   `json:"t_dtw"`
			Probability float64  `json:"probability"`
		} `json:"words"`
	} `json:"segments"`
}

// languageCodes maps whisper's full language names to ISO 639-1 codes for the
// languages we expect; unknown names are returned unchanged.
var languageCodes = map[string]string{
	"english": "en", "portuguese": "pt", "spanish": "es", "french": "fr", "german": "de",
	"italian": "it", "japanese": "ja", "korean": "ko", "chinese": "zh", "russian": "ru",
	"polish": "pl", "turkish": "tr", "dutch": "nl", "arabic": "ar", "swedish": "sv",
}

// ParseVerboseJSON converts a verbose_json response into a domain transcript,
// merging sub-word tokens into words.
func ParseVerboseJSON(body []byte, src TimestampSource) (domain.Transcript, error) {
	var v verboseJSON
	if err := json.Unmarshal(body, &v); err != nil {
		return domain.Transcript{}, fmt.Errorf("decode whisper verbose_json: %w", err)
	}
	tr := domain.Transcript{Language: detectLanguage(v)}

	var cur *domain.Word
	var probSum float64
	var probN int
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = strings.TrimSpace(cur.Text)
		if probN > 0 {
			cur.Prob = probSum / float64(probN)
		}
		if cur.Text != "" && isWordLike(cur.Text) {
			if cur.End < cur.Start {
				cur.End = cur.Start
			}
			tr.Words = append(tr.Words, *cur)
		}
		cur, probSum, probN = nil, 0, 0
	}

	for _, seg := range v.Segments {
		for _, tok := range seg.Words {
			text := tok.Word
			if isSpecialToken(text) {
				continue
			}
			if tok.Start == nil || tok.End == nil {
				return domain.Transcript{}, fmt.Errorf("whisper response has no token timestamps (token %q)", text)
			}
			start := secs(*tok.Start)
			end := secs(*tok.End)
			if src == TimestampsDTW && tok.TDTW != nil && *tok.TDTW >= 0 {
				start = time.Duration(*tok.TDTW) * 10 * time.Millisecond
			}
			// A token starting with a space (or the first token of a segment)
			// begins a new word; punctuation-only tokens attach to the previous word.
			startsWord := cur == nil || strings.HasPrefix(text, " ")
			if startsWord && (cur == nil || !isPunctOnly(text)) {
				flush()
				cur = &domain.Word{Text: text, Start: start, End: end}
			} else {
				cur.Text += text
				if end > cur.End {
					cur.End = end
				}
			}
			probSum += tok.Probability
			probN++
		}
		flush() // words never span segments
	}
	flush()
	return tr, nil
}

func detectLanguage(v verboseJSON) string {
	best, bestP := "", -1.0
	for code, p := range v.LanguageProbabilities {
		if p > bestP || (p == bestP && code < best) {
			best, bestP = code, p
		}
	}
	if best != "" {
		return best
	}
	name := strings.ToLower(strings.TrimSpace(v.Language))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(v.DetectedLanguage))
	}
	if code, ok := languageCodes[name]; ok {
		return code
	}
	return name
}

func secs(f float64) time.Duration {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return time.Duration(math.Round(f * float64(time.Second)))
}

// isSpecialToken reports whisper control tokens such as "[_BEG_]" or "[_TT_150]".
func isSpecialToken(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "[_") && strings.HasSuffix(t, "]")
}

func isPunctOnly(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	for _, r := range t {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			return false
		}
	}
	return true
}

func isWordLike(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
