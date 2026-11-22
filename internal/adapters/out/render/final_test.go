package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func ms(v int) time.Duration { return time.Duration(v) * time.Millisecond }

func sampleSpec() domain.RenderSpec {
	return domain.RenderSpec{
		ClipID: "0193f1c2-aaaa-7bbb-8ccc-123456789abc", Streamer: "streamer_a",
		Start: ms(39650), End: ms(70500), Width: 1080, Height: 1920, FPS: 30, Layout: domain.LayoutBlur,
		Headline: "Ele pulou da ponte e caiu em cima da viatura!",
		Captions: []domain.Caption{
			// Same language: karaoke from the word timings.
			{Start: ms(41000), End: ms(42600), Text: "no way, did you see"},
			// Translation, long: split in two events, proportional timing.
			{Start: ms(43000), End: ms(48000), Text: "Ele pulou da ponte e caiu bem em cima da viatura da polícia, que loucura {total}"},
		},
		Words: []domain.Word{
			{Text: "No", Start: ms(41000), End: ms(41200)}, {Text: "way,", Start: ms(41250), End: ms(41600)},
			{Text: "did", Start: ms(41700), End: ms(41900)}, {Text: "you", Start: ms(41950), End: ms(42100)},
			{Text: "see", Start: ms(42150), End: ms(42500)},
			{Text: "he", Start: ms(43000), End: ms(43200)},
		},
		MaxBytes: 45 << 20,
	}
}

func TestBuildASSGolden(t *testing.T) {
	golden(t, "render_subtitles", BuildASS(sampleSpec(), ASSStyle{Font: "Noto Sans", MaxLineChars: 28, HeadlineLineChars: 24}))
}

func TestBuildASSHeadlineOnTheStackSeam(t *testing.T) {
	spec := sampleSpec()
	spec.Layout = domain.LayoutStack
	if ass := BuildASS(spec, ASSStyle{}); !strings.Contains(ass, `Headline,,0,0,0,,{\an5\pos(540,672)}`) {
		t.Fatalf("stack headline must sit on the camera/game seam (y=672):\n%s", ass)
	}
	spec.Layout = domain.LayoutBlur
	if ass := BuildASS(spec, ASSStyle{}); strings.Contains(ass, `\pos(`) {
		t.Fatalf("blur headline keeps the style position:\n%s", ass)
	}
}

func TestFilterGraphsGolden(t *testing.T) {
	stack := domain.StackRegions{Facecam: domain.Rect{X: 0.7, Y: 0, W: 0.3, H: 0.3}, Gameplay: domain.Rect{X: 0, Y: 0, W: 1, H: 1}}
	var b strings.Builder
	for _, l := range []domain.Layout{domain.LayoutBlur, domain.LayoutStack, domain.LayoutCrop} {
		b.WriteString(string(l) + ": " + filterGraph(l, stack, 1080, 1920, 30, ms(39650), ms(70500), "/output/.x.ass") + "\n")
	}
	golden(t, "render_layouts", b.String())
}

func TestFinalArgsGolden(t *testing.T) {
	spec := sampleSpec()
	graph := filterGraph(spec.Layout, spec.Stack, 1080, 1920, 30, spec.Start, spec.End, "/output/.x.ass")
	golden(t, "render_final_nvenc", lines(finalArgs("/pending/c.ts", "/output/c.mp4", graph, spec, EncoderNVENC, 4, 1)))
	golden(t, "render_final_x264", lines(finalArgs("/pending/c.ts", "/output/c.mp4", graph, spec, EncoderX264, 4, 0.8)))
}

func TestASSHelpers(t *testing.T) {
	if assTime(ms(3723456)) != "1:02:03.46" || assTime(-time.Second) != "0:00:00.00" {
		t.Fatalf("assTime = %s", assTime(ms(3723456)))
	}
	if in := "a {b} \\c\nd"; assText(in) != "a (b) /c d" {
		t.Fatalf("assText = %q", assText(in))
	}
	lines := wrapWords(strings.Fields("Gente, vocês viram isso? Ele pulou da ponte"), 16)
	if len(lines) != 4 || strings.Join(lines[0], " ") != "Gente, vocês" || strings.Join(lines[2], " ") != "Ele pulou" || strings.Join(lines[3], " ") != "da ponte" {
		t.Fatalf("wrap = %v", lines)
	}
	// Karaoke durations always add up to the caption duration.
	c := domain.Caption{Start: time.Second, End: 4 * time.Second, Text: "um dois tres quatro"}
	var sum time.Duration
	for _, d := range karaoke(strings.Fields(c.Text), c, nil) {
		sum += d
	}
	if sum != 3*time.Second {
		t.Fatalf("karaoke sum = %v", sum)
	}
	if got := karaoke(nil, c, nil); len(got) != 0 {
		t.Fatal("empty karaoke")
	}
	if captionFontSize(1080, 28) != 56 {
		t.Fatalf("font size = %d", captionFontSize(1080, 28))
	}
}

func TestVideoMaxRate(t *testing.T) {
	// 45 MiB over 60 s: 0.92 × 47185920 × 8 / 60 − 160k ≈ 5.63 Mbit/s.
	if r := videoMaxRate(45<<20, time.Minute, 1); r < 5_500_000 || r > 5_700_000 {
		t.Fatalf("rate = %d", r)
	}
	if videoMaxRate(45<<20, 5*time.Second, 1) != 12_000_000 || videoMaxRate(1<<20, time.Hour, 1) != 1_000_000 || videoMaxRate(0, 0, 1) != 8_000_000 {
		t.Fatal("clamps")
	}
	if got := outputName(sampleSpec(), time.Date(2026, 11, 21, 18, 4, 5, 0, time.UTC)); got != "20261121-180405_streamer_a_6789abc" && got != "20261121-180405_streamer_a_56789abc" {
		t.Fatalf("output name = %s", got)
	}
}

func TestFinalRetriesOnSize(t *testing.T) {
	out := t.TempDir()
	f := &fakeFFmpeg{write: 2000}
	r := New(context.Background(), Config{Encoder: EncoderX264, OutputDir: out}, withRunner(f.run),
		withNow(func() time.Time { return time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC) }))
	spec := sampleSpec()
	spec.MaxBytes = 1500
	if _, err := r.Final(context.Background(), "/pending/c.ts", spec); err == nil {
		t.Fatal("expected size error")
	}
	if n := len(f.calls); n != 2 {
		t.Fatalf("ffmpeg runs = %d, want 2 (retry at 80%%)", n)
	}
	spec.MaxBytes = 45 << 20
	ref, err := r.Final(context.Background(), "/pending/c.ts", spec)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(string(ref)) != out || !strings.HasSuffix(string(ref), ".mp4") {
		t.Fatalf("ref = %s", ref)
	}
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ass") {
			t.Fatal("temporary subtitles left behind")
		}
	}
	spec.End = spec.Start
	if _, err := r.Final(context.Background(), "/pending/c.ts", spec); err == nil {
		t.Fatal("expected error for empty cut")
	}
}

func TestCamBandAspect(t *testing.T) {
	if got := CamBandAspect(1080, 1920); got != 1080.0/672.0 {
		t.Fatalf("aspect = %v", got)
	}
	if CamBandAspect(1080, 0) != 0 {
		t.Fatal("zero height")
	}
}
