// Package facecam implements the FacecamDetector port: it finds the
// streamer's camera in a retained window from a dozen low-resolution
// keyframes, on the CPU only (no GPU: whisper and NVENC keep it), with the
// pure-Go pigo face detector (ADR 0027).
package facecam

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

// Config configures the detector.
type Config struct {
	FFmpeg string
	// Frames is how many keyframes are analysed (default 12).
	Frames int
	// Threads bounds ffmpeg's decoding threads (default 2).
	Threads int
	// CamAspect is the width/height of the camera band of the output
	// (default 1080/672, the stack layout), used for face-proportional crops.
	CamAspect float64
}

// Detector finds the camera; at most one detection runs at a time.
type Detector struct {
	cfg   Config
	sem   chan struct{}
	run   func(ctx context.Context, args ...string) ([]byte, error)
	faces faceFinder
	log   *slog.Logger
}

var _ app.FacecamDetector = (*Detector)(nil)

// Option configures a Detector.
type Option func(*Detector)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(d *Detector) { d.log = l } }

// withRunner replaces ffmpeg (tests).
func withRunner(f func(ctx context.Context, args ...string) ([]byte, error)) Option {
	return func(d *Detector) { d.run = f }
}

// withFaceFinder replaces the face detector (tests).
func withFaceFinder(f faceFinder) Option { return func(d *Detector) { d.faces = f } }

// New builds the detector (it parses pigo's embedded cascade once).
func New(cfg Config, opts ...Option) *Detector {
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.Frames <= 0 {
		cfg.Frames = 12
	}
	if cfg.Threads <= 0 {
		cfg.Threads = 2
	}
	if cfg.CamAspect <= 0 {
		cfg.CamAspect = 1080.0 / 672.0
	}
	d := &Detector{cfg: cfg, sem: make(chan struct{}, 1), log: slog.Default()}
	d.run = func(ctx context.Context, args ...string) ([]byte, error) {
		return proc.Run(ctx, nil, cfg.FFmpeg, args...)
	}
	for _, o := range opts {
		o(d)
	}
	if d.faces == nil {
		d.faces = pigoFinder()
	}
	return d
}

// extractArgs decodes only keyframes (cheap: no inter-frame decoding), takes
// every 4th one so the frames span the whole window, and writes them as raw
// 640x360 gray images.
func extractArgs(src string, frames, threads int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-threads", strconv.Itoa(threads),
		"-skip_frame", "nokey",
		"-i", src,
		"-an",
		"-vf", `select='not(mod(n\,4))',scale=640:360,format=gray`,
		"-fps_mode", "vfr",
		"-frames:v", strconv.Itoa(frames),
		"-f", "rawvideo", "pipe:1",
	}
}

// Detect finds the camera in the media. ok is false when no camera is
// clearly visible (too few frames, no stable face, or a face that looks like
// game content without a camera box).
func (d *Detector) Detect(ctx context.Context, src domain.MediaRef) (domain.Rect, bool, error) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return domain.Rect{}, false, ctx.Err()
	}
	defer func() { <-d.sem }()
	raw, err := d.run(ctx, extractArgs(string(src), d.cfg.Frames, d.cfg.Threads)...)
	if err != nil {
		return domain.Rect{}, false, fmt.Errorf("extract keyframes of %s: %w", src, err)
	}
	size := frameW * frameH
	var frames [][]uint8
	for i := 0; i+size <= len(raw); i += size {
		frames = append(frames, raw[i:i+size])
	}
	if len(frames) < 3 {
		d.log.Debug("facecam: too few keyframes", slog.String("media", string(src)), slog.Int("frames", len(frames)))
		return domain.Rect{}, false, nil
	}
	res := analyze(frames, d.faces, d.cfg.CamAspect)
	d.log.Debug("facecam analysis", slog.String("media", string(src)), slog.String("result", res.reason),
		slog.Int("face_hits", res.hits), slog.Int("frames", len(frames)), slog.Any("sides", res.sides))
	return res.rect, res.ok, nil
}
