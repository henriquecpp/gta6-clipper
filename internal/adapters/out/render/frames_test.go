package render

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFramesArgs(t *testing.T) {
	got := strings.Join(framesArgs("/pending/x.ts", "/tmp/f/%03d.jpg", 3100*time.Millisecond, 40, 4), " ")
	want := "-hide_banner -loglevel error -y -skip_frame nokey -i /pending/x.ts -an -sn " +
		"-vf fps=fps=1000/3100:start_time=0:round=up,scale=640:-2 -frames:v 40 -q:v 4 -threads 4 /tmp/f/%03d.jpg"
	if got != want {
		t.Fatalf("args:\n got %s\nwant %s", got, want)
	}
}

func TestFrames(t *testing.T) {
	var pattern string
	write := 3
	r := New(context.Background(), Config{Encoder: EncoderX264}, WithLogger(slog.New(slog.DiscardHandler)),
		withRunner(func(_ context.Context, args ...string) ([]byte, error) {
			pattern = args[len(args)-1]
			for i := 1; i <= write; i++ {
				if err := os.WriteFile(strings.Replace(pattern, "%03d", fmt.Sprintf("%03d", i), 1), []byte{byte(i)}, 0o600); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}))
	frames, err := r.Frames(context.Background(), "/pending/x.ts", 3*time.Second, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0].At != 0 || frames[2].At != 6*time.Second || frames[1].JPEG[0] != 2 {
		t.Fatalf("frames = %+v", frames)
	}
	if _, err := os.Stat(strings.TrimSuffix(pattern, "%03d.jpg")); !os.IsNotExist(err) {
		t.Fatalf("temporary directory left behind: %v", err)
	}

	write = 0
	if _, err := r.Frames(context.Background(), "x", time.Second, 5); err == nil || !strings.Contains(err.Error(), "no frame") {
		t.Fatalf("no frames: %v", err)
	}
	if _, err := r.Frames(context.Background(), "x", 0, 5); err == nil {
		t.Fatal("zero interval accepted")
	}
	failing := New(context.Background(), Config{Encoder: EncoderX264}, withRunner(func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("invalid data")
	}))
	if _, err := failing.Frames(context.Background(), "x", time.Second, 5); err == nil || !strings.Contains(err.Error(), "invalid data") {
		t.Fatalf("ffmpeg error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failing.finals <- struct{}{} // busy
	if _, err := failing.Frames(ctx, "x", time.Second, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
