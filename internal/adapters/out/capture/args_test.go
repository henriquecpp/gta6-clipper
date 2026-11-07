package capture

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

// golden compares got with testdata/golden/<name>.golden (repo root).
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "testdata", "golden", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
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

func lines(args []string) string { return strings.Join(args, "\n") + "\n" }

func TestArgsGolden(t *testing.T) {
	golden(t, "capture_streamlink", lines(streamlinkArgs("some_streamer", "720p60")))
	golden(t, "capture_ffmpeg", lines(ffmpegCaptureArgs("/buffer/s1", 3, 2*time.Second, 16000)))
	golden(t, "capture_concat", lines(concatArgs("/pending/.concat-1.txt", "/pending/out.ts", 4)))
}

func TestQualitySpec(t *testing.T) {
	cases := map[string]string{"720p60": "720p60,720p,720p30,best", "1080p60": "1080p60,1080p,1080p30,best", "720p30": "720p30,720p,best", "480p": "480p,best", "best": "best", "": "best"}
	for in, want := range cases {
		if got := qualitySpec(in); got != want {
			t.Errorf("qualitySpec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConcatList(t *testing.T) {
	got := concatList([]string{"/buffer/a.ts", "/buffer/it's.ts"})
	want := "file '/buffer/a.ts'\nfile '/buffer/it'\\''s.ts'\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseSegmentLine(t *testing.T) {
	name, start, end, err := parseSegmentLine("r001_000000004.ts,10.010000,12.012000\n")
	if err != nil || name != "r001_000000004.ts" || start != 10.01 || end != 12.012 {
		t.Fatalf("got %q %v %v %v", name, start, end, err)
	}
	if name, _, _, err := parseSegmentLine("a,b.ts,1,2"); err != nil || name != "a,b.ts" {
		t.Fatalf("comma in name: %q %v", name, err)
	}
	for _, bad := range []string{"", "x.ts,1", "x.ts,a,2", "x.ts,3,2"} {
		if _, _, _, err := parseSegmentLine(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
