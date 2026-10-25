package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/whisper"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

// gpuCheck validates, inside the app container: (1) h264_nvenc is listed and
// can encode; (2) whisper-server returns word timestamps, and measures their
// error against the TTS ground truth in testdata/speech.
func gpuCheck(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gpucheck", flag.ContinueOnError)
	sample := fs.String("sample", "/usr/local/share/clipper/speech/en_sample", "path prefix of <sample>.wav and <sample>.words.json")
	whisperURL := fs.String("whisper", envOr("WHISPER_URL", "http://whisper:8080"), "whisper-server base URL")
	skipNVENC := fs.Bool("skip-nvenc", false, "skip the NVENC check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var errs []error
	if !*skipNVENC {
		if err := checkNVENC(ctx, out); err != nil {
			errs = append(errs, err)
		}
	}
	if err := checkWordTimestamps(ctx, out, *whisperURL, *sample); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "OK: all GPU checks passed")
	return nil
}

func checkNVENC(ctx context.Context, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "== ffmpeg NVENC")
	encoders, err := proc.Run(ctx, nil, "ffmpeg", "-hide_banner", "-encoders")
	if err != nil {
		return fmt.Errorf("list encoders: %w", err)
	}
	if !bytes.Contains(encoders, []byte("h264_nvenc")) {
		return errors.New("FAIL: h264_nvenc not listed by ffmpeg -encoders")
	}
	_, err = proc.Run(ctx, nil, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30", "-t", "2",
		"-c:v", "h264_nvenc", "-f", "null", "-")
	if err != nil {
		return fmt.Errorf("FAIL: NVENC encode: %w", err)
	}
	_, _ = fmt.Fprintln(out, "OK: h264_nvenc encoded 2 s of 720p30")
	return nil
}

type truthWord struct {
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
}

func checkWordTimestamps(ctx context.Context, out io.Writer, url, sample string) error {
	_, _ = fmt.Fprintln(out, "== whisper word timestamps")
	wav, err := os.ReadFile(sample + ".wav")
	if err != nil {
		return fmt.Errorf("read sample: %w", err)
	}
	truthRaw, err := os.ReadFile(sample + ".words.json")
	if err != nil {
		return fmt.Errorf("read ground truth: %w", err)
	}
	var truth []truthWord
	if err := json.Unmarshal(truthRaw, &truth); err != nil {
		return fmt.Errorf("decode ground truth: %w", err)
	}

	c := whisper.NewClient(url, nil)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := c.Health(ctx); err == nil {
			break
		} else if time.Now().After(deadline) {
			return fmt.Errorf("FAIL: whisper not healthy: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	start := time.Now()
	body, err := c.Inference(ctx, bytes.NewReader(wav), whisper.InferenceOptions{Language: "auto"})
	if err != nil {
		return fmt.Errorf("FAIL: inference: %w", err)
	}
	_, _ = fmt.Fprintf(out, "inference took %s\n", time.Since(start).Round(time.Millisecond))

	for _, src := range []whisper.TimestampSource{whisper.TimestampsToken, whisper.TimestampsDTW} {
		tr, err := whisper.ParseVerboseJSON(body, src)
		if err != nil {
			return fmt.Errorf("FAIL: %w", err)
		}
		st := compareWords(truth, tr.Words)
		_, _ = fmt.Fprintf(out, "[%s] language=%s words=%d matched=%d/%d mean_abs_err=%s p50=%s p90=%s max=%s\n",
			src, tr.Language, len(tr.Words), st.matched, len(truth), st.mean, st.p50, st.p90, st.max)
		if src == whisper.TimestampsToken && st.matched < len(truth)/2 {
			return fmt.Errorf("FAIL: only %d of %d words matched", st.matched, len(truth))
		}
	}
	return nil
}

type wordStats struct {
	matched             int
	mean, p50, p90, max time.Duration
}

// compareWords aligns ground-truth words with transcribed words in order and
// reports the absolute error of word start times.
func compareWords(truth []truthWord, got []domain.Word) wordStats {
	var diffs []time.Duration
	j := 0
	for _, tw := range truth {
		want := normWord(tw.Text)
		for k := j; k < len(got) && k < j+6; k++ {
			if normWord(got[k].Text) == want {
				d := got[k].Start - time.Duration(tw.StartMS)*time.Millisecond
				diffs = append(diffs, time.Duration(math.Abs(float64(d))))
				j = k + 1
				break
			}
		}
	}
	st := wordStats{matched: len(diffs)}
	if len(diffs) == 0 {
		return st
	}
	slices.Sort(diffs)
	var sum time.Duration
	for _, d := range diffs {
		sum += d
	}
	st.mean = (sum / time.Duration(len(diffs))).Round(time.Millisecond)
	st.p50 = diffs[len(diffs)/2].Round(time.Millisecond)
	st.p90 = diffs[int(float64(len(diffs)-1)*0.9)].Round(time.Millisecond)
	st.max = diffs[len(diffs)-1].Round(time.Millisecond)
	return st
}

func normWord(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
