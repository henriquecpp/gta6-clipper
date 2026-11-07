package capture

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// qualityRe splits Twitch variant names such as "720p60" into "720p" + "60".
var qualityRe = regexp.MustCompile(`^(\d+p)(\d+)$`)

// qualitySpec builds Streamlink's comma-separated fallback list. Twitch names
// variants "720p60", "720p" or "720p30" depending on the channel (observed on
// live channels on 2026-11-07), so "720p60" → "720p60,720p,720p30,best".
func qualitySpec(q string) string {
	q = strings.TrimSpace(q)
	if q == "" || q == "best" {
		return "best"
	}
	parts := []string{q}
	if m := qualityRe.FindStringSubmatch(q); m != nil {
		parts = append(parts, m[1])
		if m[2] != "30" {
			parts = append(parts, m[1]+"30")
		}
	}
	return strings.Join(append(parts, "best"), ",")
}

// streamlinkArgs pipes a Twitch live stream to stdout with low latency
// (Streamlink 8.6.3 CLI, streamlink.github.io/cli.html).
func streamlinkArgs(login, quality string) []string {
	return []string{
		"--stdout",
		"--loglevel", "warning",
		"--twitch-low-latency",
		"--hls-live-edge", "2",
		"--stream-segment-threads", "2",
		"--retry-open", "2",
		"--stream-timeout", "30",
		"twitch.tv/" + login,
		qualitySpec(quality),
	}
}

// segmentPattern is the file name pattern of the segments of one run (always
// with forward slashes, which ffmpeg accepts on every platform).
func segmentPattern(dir string, run int) string {
	return filepath.ToSlash(filepath.Join(dir, fmt.Sprintf("r%03d_%%09d.ts", run)))
}

// ffmpegCaptureArgs reads MPEG-TS from stdin and writes (a) segments without
// re-encoding, listing each finished segment as CSV on fd 3, and (b) 16 kHz
// mono s16le PCM on stdout for the VAD.
func ffmpegCaptureArgs(segDir string, run int, segDur time.Duration, sampleRate int) []string {
	return []string{
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "+genpts+discardcorrupt",
		"-i", "pipe:0",
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", strconv.FormatFloat(segDur.Seconds(), 'f', -1, 64),
		"-segment_format", "mpegts",
		"-reset_timestamps", "0",
		"-segment_list", "pipe:3",
		"-segment_list_type", "csv",
		segmentPattern(segDir, run),
		"-map", "0:a:0",
		"-vn",
		"-af", "aresample=async=1",
		"-ac", "1",
		"-ar", strconv.Itoa(sampleRate),
		"-f", "s16le",
		"pipe:1",
	}
}

// concatArgs copies the listed segments into one MPEG-TS file without
// re-encoding; the concat demuxer makes timestamps continuous.
func concatArgs(listPath, out string, threads int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-threads", strconv.Itoa(max(threads, 1)),
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-map", "0",
		"-c", "copy",
		"-f", "mpegts",
		"-y", out,
	}
}

// concatList is the content of the concat demuxer list file.
func concatList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		// Single quotes are escaped as '\'' in concat lists.
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(filepath.ToSlash(p), "'", `'\''`))
	}
	return b.String()
}

// parseSegmentLine parses a CSV entry of the segment muxer:
// "name.ts,start_seconds,end_seconds".
func parseSegmentLine(line string) (name string, start, end float64, err error) {
	parts := strings.Split(strings.TrimSpace(line), ",")
	if len(parts) < 3 {
		return "", 0, 0, fmt.Errorf("segment list: malformed line %q", line)
	}
	n := len(parts)
	start, err1 := strconv.ParseFloat(parts[n-2], 64)
	end, err2 := strconv.ParseFloat(parts[n-1], 64)
	if err1 != nil || err2 != nil || end < start {
		return "", 0, 0, fmt.Errorf("segment list: bad times in %q", line)
	}
	// File names may contain commas; everything before the two times is the name.
	return strings.Join(parts[:n-2], ","), start, end, nil
}
