package render

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "testdata", "golden", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if string(want) != got {
		t.Fatalf("%s mismatch\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func lines(a []string) string { return strings.Join(a, "\n") + "\n" }

var previewSpec = domain.PreviewSpec{Start: 33*time.Second + 333*time.Millisecond, Duration: 40 * time.Second,
	Width: 480, Height: 854, FPS: 30, Layout: domain.LayoutBlur, MaxBytes: 45 << 20}

var previewGraph = filterGraph(previewSpec.Layout, previewSpec.Stack, previewSpec.Width, previewSpec.Height, previewSpec.FPS, "")

func TestPreviewArgsGolden(t *testing.T) {
	golden(t, "render_preview_nvenc", lines(previewArgs("/pending/s1_1_2.ts", "/pending/s1_1_2.preview.mp4", previewGraph, previewSpec, EncoderNVENC, 4)))
	golden(t, "render_preview_x264", lines(previewArgs("/pending/s1_1_2.ts", "/pending/s1_1_2.preview.mp4", previewGraph, previewSpec, EncoderX264, 4)))
}

func TestPreviewPath(t *testing.T) {
	cases := map[domain.MediaRef]string{"/pending/a.ts": "/pending/a.preview.mp4", "/pending/x.y/a": "/pending/x.y/a.preview.mp4"}
	for in, want := range cases {
		if got := previewPath(in); got != want {
			t.Errorf("previewPath(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeFFmpeg struct {
	mu       sync.Mutex
	calls    [][]string
	encoders string
	failTest bool
	failAll  bool
	write    int // bytes written to the output file
}

func (f *fakeFFmpeg) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if slices.Contains(args, "-encoders") {
		return []byte(f.encoders), nil
	}
	if slices.Contains(args, "testsrc2=size=640x360:rate=30") && f.failTest {
		return nil, errors.New("Cannot load libnvidia-encode.so.1")
	}
	if f.failAll {
		return nil, errors.New("ffmpeg failed")
	}
	if out := args[len(args)-1]; strings.HasSuffix(out, ".mp4") {
		return nil, os.WriteFile(out, make([]byte, f.write), 0o600)
	}
	return nil, nil
}

func newTestRenderer(f *fakeFFmpeg, enc string) *Renderer {
	return New(context.Background(), Config{Encoder: enc, Threads: 2}, withRunner(f.run), WithLogger(slog.New(slog.DiscardHandler)))
}

func TestEncoderDetection(t *testing.T) {
	ok := &fakeFFmpeg{encoders: " V....D h264_nvenc  NVIDIA NVENC H.264 encoder"}
	if r := newTestRenderer(ok, EncoderNVENC); !r.NVENC() || r.Encoder() != EncoderNVENC {
		t.Fatalf("encoder = %s", r.Encoder())
	}
	missing := &fakeFFmpeg{encoders: " V....D libx264"}
	if r := newTestRenderer(missing, EncoderNVENC); r.NVENC() {
		t.Fatal("nvenc not listed → libx264")
	}
	broken := &fakeFFmpeg{encoders: "h264_nvenc", failTest: true}
	if r := newTestRenderer(broken, EncoderNVENC); r.NVENC() {
		t.Fatal("nvenc test encode failed → libx264")
	}
	cpu := &fakeFFmpeg{}
	if r := newTestRenderer(cpu, EncoderX264); r.NVENC() || len(cpu.calls) != 0 {
		t.Fatal("libx264 preferred → no detection")
	}
}

func TestPreviewRendersAndChecksSize(t *testing.T) {
	dir := t.TempDir()
	src := domain.MediaRef(filepath.Join(dir, "clip.ts"))
	f := &fakeFFmpeg{write: 1000}
	r := newTestRenderer(f, EncoderX264)
	ref, err := r.Preview(context.Background(), src, previewSpec)
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != filepath.Join(dir, "clip.preview.mp4") {
		t.Fatalf("ref = %s", ref)
	}

	// Too big.
	f.write = 2000
	spec := previewSpec
	spec.MaxBytes = 1500
	if _, err := r.Preview(context.Background(), src, spec); err == nil {
		t.Fatal("expected size error")
	}
	if _, err := os.Stat(string(ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized output must be removed")
	}
	// Empty output.
	f.write = 0
	if _, err := r.Preview(context.Background(), src, previewSpec); err == nil {
		t.Fatal("expected empty output error")
	}
	// ffmpeg failure.
	f.failAll = true
	if _, err := r.Preview(context.Background(), src, previewSpec); err == nil {
		t.Fatal("expected ffmpeg error")
	}
	// Missing frame size.
	f.failAll = false
	if _, err := r.Preview(context.Background(), src, domain.PreviewSpec{Duration: time.Second}); err == nil {
		t.Fatal("expected size error for a 0x0 preview")
	}
	// Cancelled while waiting for a worker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.previews <- struct{}{}
	if _, err := r.Preview(ctx, src, previewSpec); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
