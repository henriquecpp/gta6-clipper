//go:build integration

package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

const sampleVideo = "../../../../testdata/video/sample.mp4"

// fileSource replays the sample video as if it were the live stream, 4× faster
// than real time.
func fileSource(ctx context.Context, _ domain.LiveStream) *exec.Cmd {
	return proc.Command(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-readrate", "4",
		"-i", sampleVideo, "-c", "copy", "-f", "mpegts", "pipe:1")
}

func probeDuration(t *testing.T, path string) time.Duration {
	t.Helper()
	out, err := proc.Run(context.Background(), nil, "ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe output %q: %v", out, err)
	}
	return time.Duration(f * float64(time.Second))
}

func TestCaptureIntegration(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{
		BufferDir: filepath.Join(dir, "buffer"), PendingDir: filepath.Join(dir, "pending"),
		BufferDuration: 10 * time.Minute, MaxBytes: 1 << 30, SegmentDuration: 2 * time.Second,
		Frame: 30 * time.Millisecond, Threads: 2, Workers: 1,
	}, WithSource(fileSource), WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	live := domain.LiveStream{ID: "s1", Login: "sample"}

	var frames []domain.AudioFrame
	err = c.Capture(ctx, live, time.Now(), func(f domain.AudioFrame) { frames = append(frames, f) })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Capture = %v, want io.EOF at the end of the source", err)
	}

	// 40 s of audio in 30 ms frames.
	if n := len(frames); n < 1250 || n > 1400 {
		t.Fatalf("audio frames = %d, want ~1333", n)
	}
	speech := func(from, to time.Duration) float64 {
		s, n := 0, 0
		for _, f := range frames {
			p := time.Duration(f.Pos)
			if p >= from && p < to {
				n++
				if f.Speech {
					s++
				}
			}
		}
		return float64(s) / float64(max(n, 1))
	}
	if r := speech(9*time.Second, 25*time.Second); r < 0.5 {
		t.Errorf("speech ratio during speech = %.2f", r)
	}
	if r := speech(1*time.Second, 7*time.Second); r > 0.1 {
		var b strings.Builder
		for s := 0; s < 14; s++ {
			fmt.Fprintf(&b, "%ds=%.2f ", s, speech(time.Duration(s)*time.Second, time.Duration(s+1)*time.Second))
		}
		t.Errorf("speech ratio before speech = %.2f; per second: %s; first frames: %+v", r, b.String(), frames[:5])
	}

	segs, bytes := c.Stats()
	if segs < 15 || segs > 25 || bytes <= 0 {
		t.Fatalf("buffer = %d segments, %d bytes", segs, bytes)
	}

	ref, w, err := c.RetainWindow(ctx, live.ID, domain.StreamPos(10*time.Second), domain.StreamPos(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w.From > domain.StreamPos(10*time.Second) || w.To < domain.StreamPos(20*time.Second) {
		t.Fatalf("retained window %v–%v does not cover 10–20 s", w.From, w.To)
	}
	got := probeDuration(t, string(ref))
	if diff := got - w.Duration(); diff < -700*time.Millisecond || diff > 700*time.Millisecond {
		t.Fatalf("retained file lasts %v, window says %v", got, w.Duration())
	}

	// Not yet captured.
	if _, _, err := c.RetainWindow(ctx, live.ID, domain.StreamPos(30*time.Second), domain.StreamPos(90*time.Second)); !errors.Is(err, domain.ErrWindowUnavailable) {
		t.Fatalf("future window err = %v", err)
	}

	// Prune keeps referenced files only; Delete refuses paths outside pending.
	orphan := filepath.Join(dir, "pending", "orphan.ts")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Prune(map[domain.MediaRef]bool{ref: true}); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if _, err := os.Stat(string(ref)); err != nil {
		t.Fatalf("kept file removed: %v", err)
	}
	if err := c.Delete(ctx, domain.MediaRef(filepath.Join(dir, "buffer", "x.ts"))); err == nil {
		t.Fatal("delete outside pending must fail")
	}
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatalf("deleting twice must not fail: %v", err)
	}

	c.DropStream(live.ID)
	if segs, _ := c.Stats(); segs != 0 {
		t.Fatal("buffer not dropped")
	}
}

// TestCaptureCancelCleansUp checks that cancelling a running capture stops
// both processes and deletes the buffer of the live.
func TestCaptureCancelCleansUp(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{
		BufferDir: filepath.Join(dir, "buffer"), PendingDir: filepath.Join(dir, "pending"),
		BufferDuration: time.Minute, MaxBytes: 1 << 30, SegmentDuration: 2 * time.Second,
	}, WithLogger(slog.New(slog.DiscardHandler)), WithSource(func(ctx context.Context, _ domain.LiveStream) *exec.Cmd {
		return proc.Command(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-stream_loop", "-1",
			"-i", sampleVideo, "-c", "copy", "-f", "mpegts", "pipe:1")
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	frames := 0
	go func() {
		done <- c.Capture(ctx, domain.LiveStream{ID: "s2"}, time.Now(), func(domain.AudioFrame) { frames++ })
	}()
	time.Sleep(5 * time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Capture = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("capture did not stop after cancel")
	}
	if frames == 0 {
		t.Fatal("no audio frames before cancel")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "buffer"))
	if len(entries) != 0 {
		t.Fatalf("buffer not cleaned: %v", entries)
	}
}

// A source that hangs without writing anything is stopped by the stall
// watchdog, so the supervisor can restart the capture.
func TestCaptureStalledSourceIsStopped(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{
		BufferDir: filepath.Join(dir, "buffer"), PendingDir: filepath.Join(dir, "pending"),
		BufferDuration: 10 * time.Minute, MaxBytes: 1 << 30, SegmentDuration: 2 * time.Second,
		Frame: 30 * time.Millisecond, Threads: 2, Workers: 1, StallTimeout: 2 * time.Second,
	}, WithLogger(slog.New(slog.DiscardHandler)), WithSource(func(ctx context.Context, _ domain.LiveStream) *exec.Cmd {
		return proc.Command(ctx, "sleep", "600")
	}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- c.Capture(context.Background(), domain.LiveStream{ID: "s3"}, time.Now(), func(domain.AudioFrame) {})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStalled) {
			t.Fatalf("Capture = %v, want ErrStalled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stalled capture was not stopped")
	}
}
