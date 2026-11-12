// Package render implements the Renderer port with ffmpeg: the low-resolution
// preview and (Phase 5) the final 9:16 video with burned-in ASS subtitles.
// h264_nvenc is used when available (detected at startup), with automatic
// fallback to libx264.
package render

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

// Encoders.
const (
	EncoderNVENC = "h264_nvenc"
	EncoderX264  = "libx264"
)

// Config configures the renderer.
type Config struct {
	FFmpeg string
	// Encoder is the preferred encoder (h264_nvenc or libx264).
	Encoder string
	Threads int
	// PreviewWorkers and FinalWorkers bound concurrent ffmpeg jobs (worker pools).
	PreviewWorkers int
	FinalWorkers   int
}

// runFunc runs ffmpeg with args (replaced in tests).
type runFunc func(ctx context.Context, args ...string) ([]byte, error)

// Renderer renders previews and final videos.
type Renderer struct {
	cfg      Config
	encoder  string
	log      *slog.Logger
	run      runFunc
	previews chan struct{}
	finals   chan struct{}
}

var _ app.Renderer = (*Renderer)(nil)

// Option configures a Renderer.
type Option func(*Renderer)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(r *Renderer) { r.log = l } }

// withRunner replaces the ffmpeg runner (tests).
func withRunner(f runFunc) Option { return func(r *Renderer) { r.run = f } }

// New builds the renderer and detects whether NVENC works.
func New(ctx context.Context, cfg Config, opts ...Option) *Renderer {
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	r := &Renderer{
		cfg:      cfg,
		log:      slog.Default(),
		previews: make(chan struct{}, max(cfg.PreviewWorkers, 1)),
		finals:   make(chan struct{}, max(cfg.FinalWorkers, 1)),
	}
	r.run = func(ctx context.Context, args ...string) ([]byte, error) {
		return proc.Run(ctx, nil, cfg.FFmpeg, args...)
	}
	for _, o := range opts {
		o(r)
	}
	r.encoder = r.detectEncoder(ctx)
	return r
}

// detectEncoder checks that h264_nvenc is listed and can actually encode
// (the container may lack the NVIDIA encode library or the `video` capability).
func (r *Renderer) detectEncoder(ctx context.Context) string {
	if r.cfg.Encoder != EncoderNVENC {
		return EncoderX264
	}
	out, err := r.run(ctx, "-hide_banner", "-encoders")
	if err != nil || !bytes.Contains(out, []byte(EncoderNVENC)) {
		r.log.Warn("h264_nvenc not available; falling back to libx264", slog.Any("err", err))
		return EncoderX264
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = r.run(tctx, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30",
		"-t", "1", "-c:v", EncoderNVENC, "-f", "null", "-")
	if err != nil {
		r.log.Warn("h264_nvenc test encode failed; falling back to libx264", slog.String("err", err.Error()))
		return EncoderX264
	}
	r.log.Info("video encoder selected", slog.String("encoder", EncoderNVENC))
	return EncoderNVENC
}

// Encoder returns the encoder in use.
func (r *Renderer) Encoder() string { return r.encoder }

// NVENC reports whether NVENC is in use.
func (r *Renderer) NVENC() bool { return r.encoder == EncoderNVENC }

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// previewPath is the preview file next to the source.
func previewPath(src domain.MediaRef) string {
	s := string(src)
	if i := strings.LastIndex(s, "."); i > strings.LastIndexAny(s, `/\`) {
		s = s[:i]
	}
	return s + ".preview.mp4"
}

func secs(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 3, 64) }

// previewArgs cuts [start, start+duration] of src into a small 480p MP4.
func previewArgs(src, out string, spec domain.PreviewSpec, encoder string, threads int) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", secs(spec.Start),
		"-i", src,
		"-t", secs(spec.Duration),
		"-vf", fmt.Sprintf("scale=-2:%d,format=yuv420p", spec.Height),
	}
	if encoder == EncoderNVENC {
		args = append(args, "-c:v", EncoderNVENC, "-preset", "p4", "-rc", "vbr", "-cq", "30", "-b:v", "0",
			"-maxrate", "1500k", "-bufsize", "3000k")
	} else {
		args = append(args, "-c:v", EncoderX264, "-preset", "veryfast", "-crf", "28",
			"-maxrate", "1500k", "-bufsize", "3000k")
	}
	return append(args,
		"-c:a", "aac", "-b:a", "96k", "-ac", "2",
		"-movflags", "+faststart",
		"-threads", strconv.Itoa(max(threads, 1)),
		out)
}

// Preview renders the preview of a retained window.
func (r *Renderer) Preview(ctx context.Context, src domain.MediaRef, spec domain.PreviewSpec) (domain.MediaRef, error) {
	if err := acquire(ctx, r.previews); err != nil {
		return "", err
	}
	defer func() { <-r.previews }()
	out := previewPath(src)
	if _, err := r.run(ctx, previewArgs(string(src), out, spec, r.encoder, r.cfg.Threads)...); err != nil {
		_ = os.Remove(out) // partial output
		return "", fmt.Errorf("render preview of %s: %w", src, err)
	}
	if err := checkSize(out, spec.MaxBytes); err != nil {
		_ = os.Remove(out) // unusable output
		return "", err
	}
	return domain.MediaRef(out), nil
}

// checkSize fails if the file is missing, empty or above maxBytes (> 0).
func checkSize(path string, maxBytes int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("rendered file: %w", err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("rendered file %s is empty", path)
	}
	if maxBytes > 0 && st.Size() > maxBytes {
		return fmt.Errorf("rendered file %s has %d bytes, above the %d limit", path, st.Size(), maxBytes)
	}
	return nil
}
