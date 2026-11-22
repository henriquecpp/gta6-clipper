//go:build integration || gpu

package render

import (
	"context"
	"log/slog"
	"math"
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
	spec := domain.PreviewSpec{Start: 5 * time.Second, Duration: 20 * time.Second, Width: 480, Height: 854, FPS: 30,
		Layout: domain.LayoutBlur, MaxBytes: 45 << 20}
	ref, err := r.Preview(context.Background(), src, spec)
	if err != nil {
		t.Fatal(err)
	}
	dur, w, h := probe(t, string(ref))
	if w != 480 || h != 854 || dur < 19.5 || dur > 20.5 {
		t.Fatalf("preview %dx%d %.2fs", w, h, dur)
	}
	return r
}

func TestPreviewX264(t *testing.T) {
	if r := renderPreview(t, EncoderX264); r.NVENC() {
		t.Fatal("libx264 requested")
	}
}

func renderFinal(t *testing.T, encoder string, layout domain.Layout) *Renderer {
	t.Helper()
	out := t.TempDir()
	r := New(context.Background(), Config{Encoder: encoder, Threads: 4, OutputDir: out, Style: ASSStyle{Font: "Noto Sans", MaxLineChars: 28}},
		WithLogger(slog.New(slog.DiscardHandler)))
	spec := domain.RenderSpec{
		ClipID: "it-0001", Streamer: "sample", Start: 6 * time.Second, End: 26 * time.Second,
		Width: 1080, Height: 1920, FPS: 30, Layout: layout,
		Stack:    domain.StackRegions{Facecam: domain.Rect{X: 0.7, W: 0.3, H: 0.3}, Gameplay: domain.Rect{W: 1, H: 1}},
		Headline: "Ele pulou da ponte! Ação é acentuação",
		Captions: []domain.Caption{{Start: 8 * time.Second, End: 11 * time.Second, Text: "Vocês viram isso? Não acredito"}},
		MaxBytes: 45 << 20,
	}
	ref, err := r.Final(context.Background(), copySample(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	dur, w, h := probe(t, string(ref))
	if w != 1080 || h != 1920 || dur < 19.5 || dur > 20.5 {
		t.Fatalf("final %dx%d %.2fs", w, h, dur)
	}
	return r
}

func TestFinalX264Layouts(t *testing.T) {
	for _, l := range []domain.Layout{domain.LayoutBlur, domain.LayoutStack, domain.LayoutCrop} {
		t.Run(string(l), func(t *testing.T) { renderFinal(t, EncoderX264, l) })
	}
}

// syncSource writes an MPEG-TS shaped like the Twitch captures (start time
// 1.4 s, 2 s GOP, B-frames) in which the whole frame turns white and a 1 kHz
// beep plays at the same instants: t = 1, 4, 7 and 10 s.
func syncSource(t *testing.T) domain.MediaRef {
	t.Helper()
	const flash = "between(mod(t,3),1,1.1)"
	dst := filepath.Join(t.TempDir(), "sync.ts")
	_, err := proc.Run(context.Background(), nil, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=640x360:r=30:d=12,drawbox=x=0:y=0:w=iw:h=ih:color=white:t=fill:enable='"+flash+"'",
		"-f", "lavfi", "-i", "aevalsrc='if("+flash+",0.8*sin(2*PI*1000*t),0)':s=48000:d=12",
		"-c:v", "libx264", "-g", "60", "-bf", "2", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-output_ts_offset", "1.4", "-f", "mpegts", dst)
	if err != nil {
		t.Fatal(err)
	}
	return domain.MediaRef(dst)
}

// firstFlash returns when the first white frame and the first beep of a
// rendered file are presented, in seconds.
func firstFlash(t *testing.T, path string) (video, audio float64) {
	t.Helper()
	ctx := context.Background()
	out, err := proc.Run(ctx, nil, "ffprobe", "-v", "error", "-f", "lavfi", "-i", "movie="+path+",signalstats",
		"-show_entries", "frame=pts_time:frame_tags=lavfi.signalstats.YAVG", "-of", "csv=p=0")
	if err != nil {
		t.Fatal(err)
	}
	video = -1
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pts, yavg, _ := strings.Cut(strings.TrimSpace(line), ",")
		if y, _ := strconv.ParseFloat(yavg, 64); y > 128 {
			video, _ = strconv.ParseFloat(pts, 64)
			break
		}
	}
	out, err = proc.Run(ctx, nil, "ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=start_time", "-of", "csv=p=0", path)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	pcm, err := proc.Run(ctx, nil, "ffmpeg", "-v", "error", "-i", path, "-vn", "-ac", "1", "-ar", "48000", "-f", "s16le", "pipe:1")
	if err != nil {
		t.Fatal(err)
	}
	audio = -1
	for i := 0; i+1 < len(pcm); i += 2 {
		if s := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8); s > 8000 || s < -8000 {
			audio = start + float64(i/2)/48000
			break
		}
	}
	return video, audio
}

// assertFlashAt checks that the flash and the beep both land at want.
func assertFlashAt(t *testing.T, path string, want float64) {
	t.Helper()
	v, a := firstFlash(t, path)
	const tol = 0.05 // about one frame
	if math.Abs(v-want) > tol || math.Abs(a-want) > tol {
		t.Fatalf("flash: video at %.3fs, audio at %.3fs, want both at %.3fs (A/V offset %+.0f ms)", v, a, want, 1000*(a-v))
	}
}

// The cut starts between keyframes (GOP 2 s, cut at 5.3 s): seeking in
// MPEG-TS lands on a non-key packet, which used to make the video start at
// the next keyframe while the audio started at the cut.
func TestPreviewKeepsAudioInSync(t *testing.T) {
	r := New(context.Background(), Config{Encoder: EncoderX264, Threads: 2}, WithLogger(slog.New(slog.DiscardHandler)))
	spec := domain.PreviewSpec{Start: 5300 * time.Millisecond, Duration: 4 * time.Second, Width: 480, Height: 854, FPS: 30,
		Layout: domain.LayoutBlur, MaxBytes: 45 << 20}
	ref, err := r.Preview(context.Background(), syncSource(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	assertFlashAt(t, string(ref), 1.7) // source flash at 7.0 s
}

func TestFinalKeepsAudioInSync(t *testing.T) {
	r := New(context.Background(), Config{Encoder: EncoderX264, Threads: 4, OutputDir: t.TempDir(),
		Style: ASSStyle{Font: "Noto Sans", MaxLineChars: 28}}, WithLogger(slog.New(slog.DiscardHandler)))
	spec := domain.RenderSpec{ClipID: "it-sync", Streamer: "sample", Start: 5300 * time.Millisecond, End: 9300 * time.Millisecond,
		Width: 1080, Height: 1920, FPS: 30, Layout: domain.LayoutCrop, Headline: "Sincronia", MaxBytes: 45 << 20}
	ref, err := r.Final(context.Background(), syncSource(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	assertFlashAt(t, string(ref), 1.7)
}

func TestFramesFromTheSample(t *testing.T) {
	r := New(context.Background(), Config{Encoder: EncoderX264, Threads: 2}, WithLogger(slog.New(slog.DiscardHandler)))
	src := copySample(t)
	dur, _, _ := probe(t, string(src))
	frames, err := r.Frames(context.Background(), src, 3*time.Second, 40)
	if err != nil {
		t.Fatal(err)
	}
	// One picture every 3 s over the whole sample (the last one may fall past the end).
	if want := int(dur/3) + 1; len(frames) < want-1 || len(frames) > want {
		t.Fatalf("%d frames for %.1fs, want about %d", len(frames), dur, want)
	}
	f := filepath.Join(t.TempDir(), "f.jpg")
	if err := os.WriteFile(f, frames[1].JPEG, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, w, h := probe(t, f); w != FrameWidth || h != FrameWidth*9/16 {
		t.Fatalf("frame %dx%d", w, h)
	}
	if frames[1].At != 3*time.Second {
		t.Fatalf("second frame at %v", frames[1].At)
	}
	capped, err := r.Frames(context.Background(), src, time.Second, 5)
	if err != nil || len(capped) != 5 {
		t.Fatalf("capped: %d frames, %v", len(capped), err)
	}
}
