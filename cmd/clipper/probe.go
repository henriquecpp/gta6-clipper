package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/capture"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

// probe validates the real capture pipeline against a public live channel
// (no Twitch credentials needed) and estimates the HLS latency from the
// #EXT-X-PROGRAM-DATE-TIME tags of the media playlist:
//
//	clipper probe -duration 60s <login>
func probe(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	duration := fs.Duration("duration", 60*time.Second, "how long to capture")
	quality := fs.String("quality", "720p60,720p,720p30,best", "streamlink quality list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: clipper probe [-duration 60s] <login>")
	}
	login := strings.ToLower(fs.Arg(0))
	ctx, cancel := context.WithTimeout(context.Background(), *duration+2*time.Minute)
	defer cancel()

	if err := probeLatency(ctx, out, login, *quality); err != nil {
		_, _ = fmt.Fprintf(out, "latency estimate failed: %v\n", err)
	}
	return probeCapture(ctx, out, login, *duration)
}

// probeLatency samples the media playlist and reports how old the start of
// the newest listed segment is. Low-latency Twitch playlists list a segment
// as soon as it starts; Streamlink with --hls-live-edge 2 plays about one
// segment behind it, so capture latency ≈ age + one segment, plus the
// broadcaster's encode/upload delay, which is not observable here.
func probeLatency(ctx context.Context, out io.Writer, login, quality string) error {
	raw, err := proc.Run(ctx, nil, "streamlink", "--stream-url", "--twitch-low-latency", "twitch.tv/"+login, quality)
	if err != nil {
		return err
	}
	url := strings.TrimSpace(string(raw))
	var ages []time.Duration
	var segDur time.Duration
	for range 5 {
		age, d, err := playlistEdgeAge(ctx, url)
		if err != nil {
			return err
		}
		ages = append(ages, age)
		segDur = d
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	slices.Sort(ages)
	median := ages[len(ages)/2]
	_, _ = fmt.Fprintf(out, "== HLS latency (%s)\nnewest segment start age: median %s (min %s, max %s); segment %s\nestimated capture_latency (live edge 2): %s + broadcaster encode delay\n",
		login, median.Round(10*time.Millisecond), ages[0].Round(10*time.Millisecond), ages[len(ages)-1].Round(10*time.Millisecond),
		segDur.Round(10*time.Millisecond), (median + segDur).Round(100*time.Millisecond))
	return nil
}

// playlistEdgeAge returns now − start of the newest listed segment, and its duration.
func playlistEdgeAge(ctx context.Context, url string) (time.Duration, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	now := time.Now()
	var pdt time.Time
	var last time.Time
	var lastDur time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			t, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"))
			if err == nil {
				pdt = t
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0], ",")
			if f, err := strconv.ParseFloat(v, 64); err == nil && !pdt.IsZero() {
				lastDur = time.Duration(f * float64(time.Second))
				last = pdt
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if last.IsZero() {
		return 0, 0, errors.New("playlist has no EXT-X-PROGRAM-DATE-TIME")
	}
	return now.Sub(last), lastDur, nil
}

func probeCapture(ctx context.Context, out io.Writer, login string, duration time.Duration) error {
	dir, err := os.MkdirTemp("", "clipper-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }() // scratch space
	c, err := capture.New(capture.Config{
		BufferDir: filepath.Join(dir, "buffer"), PendingDir: filepath.Join(dir, "pending"),
		BufferDuration: 10 * time.Minute, MaxBytes: 2 << 30, SegmentDuration: 2 * time.Second,
		Quality: "720p60", Frame: 30 * time.Millisecond, Threads: 2, Workers: 1,
	}, capture.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	live := domain.LiveStream{ID: domain.StreamID("probe-" + login), Login: login}
	cctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var mu sync.Mutex
	frames, speech := 0, 0
	var lastPos domain.StreamPos
	var dbSum float64
	start := time.Now()
	retained := make(chan string, 1)
	go func() {
		// Retain 20 s from the middle of the capture while it is running.
		select {
		case <-cctx.Done():
			retained <- "skipped (capture ended early)"
			return
		case <-time.After(duration - 8*time.Second):
		}
		to := domain.StreamPos(duration - 12*time.Second)
		ref, w, err := c.RetainWindow(ctx, live.ID, to-domain.StreamPos(20*time.Second), to)
		if err != nil {
			retained <- "failed: " + err.Error()
			return
		}
		d, err := proc.Run(ctx, nil, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", string(ref))
		retained <- fmt.Sprintf("%s window %v–%v (%s), ffprobe duration %s s (err %v)", filepath.Base(string(ref)), w.From, w.To, w.Duration(), strings.TrimSpace(string(d)), err)
	}()
	err = c.Capture(cctx, live, start, func(f domain.AudioFrame) {
		mu.Lock()
		defer mu.Unlock()
		frames++
		lastPos = f.Pos
		dbSum += f.DBFS
		if f.Speech {
			speech++
		}
	})
	segs, bytes := c.Stats()
	mu.Lock()
	defer mu.Unlock()
	_, _ = fmt.Fprintf(out, "== capture (%s, %s)\nresult: %v\naudio frames: %d (last at %s), speech %.0f%%, mean level %.1f dBFS\nretain: %s\nbuffer at the end: %d segments, %d bytes\n",
		login, duration, err, frames, lastPos, 100*float64(speech)/float64(max(frames, 1)), dbSum/float64(max(frames, 1)),
		<-retained, segs, bytes)
	if frames == 0 {
		return errors.New("no audio captured")
	}
	return nil
}
