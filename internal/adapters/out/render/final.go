package render

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Stack layout split: facecam on the top 35% of the frame, gameplay (most of
// the screen) below.
const stackTopShare = 0.35

// stackTop is the height of the facecam band of the stack layout (even, for
// yuv420p).
func stackTop(h int) int { return int(float64(h)*stackTopShare) / 2 * 2 }

// filterGraph builds the -filter_complex graph that arranges the 16:9 source
// in the vertical frame and burns the ASS subtitles (none when assPath is
// empty, as in previews).
//
// The chain starts with setpts=PTS-STARTPTS: with -filter_complex, ffmpeg does
// not remove the input start time (1.4 s in MPEG-TS) from the video while the
// directly mapped audio starts at 0, which would shift the video.
func filterGraph(layout domain.Layout, stack domain.StackRegions, w, h, fps int, assPath string) string {
	tail := fmt.Sprintf("fps=%d,setsar=1,format=yuv420p[v]", fps)
	if assPath != "" {
		tail = fmt.Sprintf("fps=%d,setsar=1,format=yuv420p,ass=filename='%s'[v]", fps, assPath)
	}
	switch layout {
	case domain.LayoutStack:
		top := stackTop(h)
		bot := h - top
		region := func(r domain.Rect) string {
			return fmt.Sprintf("crop=iw*%s:ih*%s:iw*%s:ih*%s", ff(r.W), ff(r.H), ff(r.X), ff(r.Y))
		}
		return fmt.Sprintf("[0:v]setpts=PTS-STARTPTS,split=2[cam][game];"+
			"[cam]%s,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d[top];"+
			"[game]%s,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d[bot];"+
			"[top][bot]vstack=inputs=2,%s",
			region(stack.Facecam), w, top, w, top, region(stack.Gameplay), w, bot, w, bot, tail)
	case domain.LayoutCrop:
		return fmt.Sprintf("[0:v]setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,%s", w, h, w, h, tail)
	default: // blur
		// The background is blurred at quarter resolution (much cheaper) and scaled back up.
		return fmt.Sprintf("[0:v]setpts=PTS-STARTPTS,split=2[bg][fg];"+
			"[bg]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=10:2,scale=%d:%d[bgb];"+
			"[fg]scale=%d:-2[fgs];"+
			"[bgb][fgs]overlay=(W-w)/2:(H-h)/2,%s",
			w/4/2*2, h/4/2*2, w/4/2*2, h/4/2*2, w, h, w, tail)
	}
}

func ff(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }

const audioBitrate = 160_000

// videoMaxRate keeps the file under maxBytes: 92% of the byte budget over the
// duration, minus the audio, clamped to [1, 12] Mbit/s.
func videoMaxRate(maxBytes int64, dur time.Duration, factor float64) int {
	if maxBytes <= 0 || dur <= 0 {
		return 8_000_000
	}
	rate := int(factor*0.92*float64(maxBytes)*8/dur.Seconds()) - audioBitrate
	return min(max(rate, 1_000_000), 12_000_000)
}

func kbps(bits int) string { return strconv.Itoa(bits/1000) + "k" }

// finalArgs renders the cut [start, end] of src into the vertical MP4.
func finalArgs(src, out, graph string, spec domain.RenderSpec, encoder string, threads int, factor float64) []string {
	dur := spec.End - spec.Start
	maxRate := videoMaxRate(spec.MaxBytes, dur, factor)
	target := min(maxRate, 6_000_000)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", secs(spec.Start),
		"-t", secs(dur),
		"-i", src,
		"-filter_complex", graph,
		"-map", "[v]", "-map", "0:a:0?",
	}
	if encoder == EncoderNVENC {
		args = append(args, "-c:v", EncoderNVENC, "-preset", "p5", "-tune", "hq", "-rc", "vbr", "-cq", "23",
			"-b:v", kbps(target), "-maxrate", kbps(maxRate), "-bufsize", kbps(2*maxRate), "-profile:v", "high")
	} else {
		args = append(args, "-c:v", EncoderX264, "-preset", "medium", "-crf", "21",
			"-maxrate", kbps(maxRate), "-bufsize", kbps(2*maxRate), "-profile:v", "high")
	}
	return append(args,
		"-c:a", "aac", "-b:a", kbps(audioBitrate), "-ac", "2", "-ar", "48000",
		"-movflags", "+faststart",
		"-threads", strconv.Itoa(max(threads, 1)),
		out)
}

// outputName is <YYYYMMDD-HHMMSS>_<streamer>_<id8>.
func outputName(spec domain.RenderSpec, now time.Time) string {
	id := string(spec.ClipID)
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return fmt.Sprintf("%s_%s_%s", now.UTC().Format("20060102-150405"), safeFile(spec.Streamer), safeFile(id))
}

func safeFile(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// Final renders the vertical video with burned-in subtitles into the output
// directory, retrying once at 80% of the bitrate if the file is too big.
func (r *Renderer) Final(ctx context.Context, src domain.MediaRef, spec domain.RenderSpec) (domain.MediaRef, error) {
	if spec.End <= spec.Start {
		return "", fmt.Errorf("render final: empty cut %v–%v", spec.Start, spec.End)
	}
	if err := acquire(ctx, r.finals); err != nil {
		return "", err
	}
	defer func() { <-r.finals }()
	if err := os.MkdirAll(r.cfg.OutputDir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	name := outputName(spec, r.now())
	assPath := filepath.ToSlash(filepath.Join(r.cfg.OutputDir, "."+name+".ass"))
	if strings.ContainsAny(assPath, `'\`) {
		return "", fmt.Errorf("render final: unsupported characters in path %q", assPath)
	}
	if err := os.WriteFile(assPath, []byte(BuildASS(spec, r.cfg.Style)), 0o644); err != nil {
		return "", fmt.Errorf("write subtitles: %w", err)
	}
	defer func() { _ = os.Remove(assPath) }() // temporary
	out := filepath.Join(r.cfg.OutputDir, name+".mp4")
	graph := filterGraph(spec.Layout, spec.Stack, spec.Width, spec.Height, spec.FPS, assPath)

	var lastErr error
	for _, factor := range []float64{1, 0.8} {
		if _, err := r.run(ctx, finalArgs(string(src), out, graph, spec, r.encoder, r.cfg.Threads, factor)...); err != nil {
			_ = os.Remove(out) // partial output
			return "", fmt.Errorf("render final of %s: %w", spec.ClipID, err)
		}
		lastErr = checkSize(out, spec.MaxBytes)
		if lastErr == nil {
			return domain.MediaRef(out), nil
		}
	}
	_ = os.Remove(out) // still too big
	return "", errors.Join(errors.New("final video above the size limit after lowering the bitrate"), lastErr)
}
