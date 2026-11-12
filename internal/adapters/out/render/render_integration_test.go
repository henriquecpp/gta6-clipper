//go:build integration || gpu

package render

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

const sampleVideo = "../../../../testdata/video/sample.mp4"

// copySample puts the sample where the preview will be written next to it.
func copySample(t *testing.T) domain.MediaRef {
	t.Helper()
	b, err := os.ReadFile(sampleVideo)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clip.ts.mp4")
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return domain.MediaRef(dst)
}

// probe returns duration (s), width and height of the first video stream.
func probe(t *testing.T, path string) (float64, int, int) {
	t.Helper()
	out, err := proc.Run(context.Background(), nil, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration", "-of", "default=nw=1", path)
	if err != nil {
		t.Fatal(err)
	}
	var dur float64
	var w, h int
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "duration":
			dur, _ = strconv.ParseFloat(v, 64)
		case "width":
			w, _ = strconv.Atoi(v)
		case "height":
			h, _ = strconv.Atoi(v)
		}
	}
	return dur, w, h
}

func renderPreview(t *testing.T, encoder string) *Renderer {
	t.Helper()
	r := New(context.Background(), Config{Encoder: encoder, Threads: 2}, WithLogger(slog.New(slog.DiscardHandler)))
	src := copySample(t)
	spec := domain.PreviewSpec{Start: 5 * time.Second, Duration: 20 * time.Second, Height: 480, MaxBytes: 45 << 20}
	ref, err := r.Preview(context.Background(), src, spec)
	if err != nil {
		t.Fatal(err)
	}
	dur, w, h := probe(t, string(ref))
	if h != 480 || w < 850 || w > 856 || dur < 19.5 || dur > 20.5 {
		t.Fatalf("preview %dx%d %.2fs", w, h, dur)
	}
	return r
}

func TestPreviewX264(t *testing.T) {
	if r := renderPreview(t, EncoderX264); r.NVENC() {
		t.Fatal("libx264 requested")
	}
}
