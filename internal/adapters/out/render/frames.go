package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

var _ app.FrameSampler = (*Renderer)(nil)

// FrameWidth is the width of the editor's frames: 640×360 for a 16:9 stream,
// about 300 image tokens each (⌈w/28⌉×⌈h/28⌉), sharp enough to tell what is
// on screen.
const FrameWidth = 640

// framesArgs decodes only the keyframes (every 2 s on Twitch: cheap, CPU
// only) and keeps one picture per interval from the start of the file, as
// JPEG files. round=up makes each picture the last keyframe at or before its
// time; start_time=0 pads the start when the video begins after the audio, so
// picture i is at i×interval.
func framesArgs(src, pattern string, interval time.Duration, limit, threads int) []string {
	rate := "1000/" + strconv.FormatInt(max(interval.Milliseconds(), 1), 10) // exact: frames per second as a ratio
	return []string{"-hide_banner", "-loglevel", "error", "-y",
		"-skip_frame", "nokey", "-i", src, "-an", "-sn",
		"-vf", fmt.Sprintf("fps=fps=%s:start_time=0:round=up,scale=%d:-2", rate, FrameWidth),
		"-frames:v", strconv.Itoa(limit), "-q:v", "4",
		"-threads", strconv.Itoa(max(threads, 1)),
		pattern}
}

// Frames implements app.FrameSampler: one JPEG every interval from the start
// of src, at most limit. It shares the final-render worker pool (it runs in
// the same job, right before the render).
func (r *Renderer) Frames(ctx context.Context, src domain.MediaRef, interval time.Duration, limit int) ([]domain.Frame, error) {
	if interval <= 0 || limit <= 0 {
		return nil, fmt.Errorf("sample frames of %s: invalid interval %v or limit %d", src, interval, limit)
	}
	if err := acquire(ctx, r.finals); err != nil {
		return nil, err
	}
	defer func() { <-r.finals }()
	dir, err := os.MkdirTemp("", "clipper-frames-")
	if err != nil {
		return nil, fmt.Errorf("sample frames: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }() // temporary
	if _, err := r.run(ctx, framesArgs(string(src), filepath.Join(dir, "%03d.jpg"), interval, limit, r.cfg.Threads)...); err != nil {
		return nil, fmt.Errorf("sample frames of %s: %w", src, err)
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if err != nil {
		return nil, fmt.Errorf("sample frames: %w", err)
	}
	slices.Sort(names) // %03d: lexical order is time order
	frames := make([]domain.Frame, 0, len(names))
	for i, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("sample frames: %w", err)
		}
		frames = append(frames, domain.Frame{At: time.Duration(i) * interval, JPEG: data})
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("sample frames of %s: no frame decoded", src)
	}
	return frames, nil
}
