package facecam

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// synth builds n frames: a smooth game scene that changes every frame and,
// when cam is not empty, a still camera box with a person who moves a bit.
func synth(n int, cam box, person box) [][]uint8 {
	frames := make([][]uint8, n)
	for i := range frames {
		f := make([]uint8, frameW*frameH)
		px, py := float64(i*37), float64(i*23)
		for y := 0; y < frameH; y++ {
			for x := 0; x < frameW; x++ {
				v := 128 + 60*math.Sin((float64(x)+px)/15)*math.Cos((float64(y)+py)/11)
				if x >= cam.x0 && x < cam.x1 && y >= cam.y0 && y < cam.y1 {
					v = float64(40 + (x*3+y*2)%50) // the room: still
					if x >= person.x0 && x < person.x1 && y >= person.y0 && y < person.y1 {
						v = 150 + float64((i*7)%15) // the person: moves a little
					}
				}
				f[y*frameW+x] = uint8(v)
			}
		}
		frames[i] = f
	}
	return frames
}

// fixedFace finds the same face in the frames where present is true.
func fixedFace(f face, present func(i int) bool) faceFinder {
	i := -1
	return func([]uint8) []face {
		i++
		if present(i) {
			return []face{f}
		}
		return nil
	}
}

func always(int) bool { return true }

const aspect = 1080.0 / 672.0

func near(a, b float64) bool { return math.Abs(a-b) <= 0.012 }

func TestAnalyzeCameraBox(t *testing.T) {
	cam := box{496, 162, 636, 266} // right side, like alanzoka's
	frames := synth(12, cam, box{530, 185, 575, 266})
	res := analyze(frames, fixedFace(face{551, 205, 44}, always), aspect)
	want := normalize(cam)
	if !res.ok || res.reason != "camera box" || !near(res.rect.X, want.X) || !near(res.rect.Y, want.Y) ||
		!near(res.rect.W, want.W) || !near(res.rect.H, want.H) {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
}

func TestAnalyzeBoxOnTheFrameEdge(t *testing.T) {
	cam := box{0, 127, 178, 225} // left edge, like facada's
	frames := synth(8, cam, box{65, 150, 110, 225})
	res := analyze(frames, fixedFace(face{87, 173, 36}, always), aspect)
	if !res.ok || res.rect.X != 0 || !near(res.rect.W, 178.0/frameW) || !near(res.rect.Y, 127.0/frameH) {
		t.Fatalf("result = %+v", res)
	}
}

func TestAnalyzeCutoutCameraGetsAFaceProportionalCrop(t *testing.T) {
	// No still box: the streamer's background is removed (like burgaofps).
	frames := synth(12, box{}, box{})
	res := analyze(frames, fixedFace(face{58, 213, 48}, always), aspect)
	if !res.ok || res.reason != "face-proportional crop" {
		t.Fatalf("result = %+v", res)
	}
	r := res.rect
	if r.X != 0 || !near(r.W, 3.4*48/frameW) || !near(r.H, 3.4*48/aspect/frameH) || !near(r.Y, (213-0.42*3.4*48/aspect)/frameH) {
		t.Fatalf("crop = %+v", r)
	}
	// The crop keeps the output aspect (width/height in pixels).
	if got := r.W * frameW / (r.H * frameH); math.Abs(got-aspect) > 0.03 {
		t.Fatalf("crop aspect = %.3f, want %.3f", got, aspect)
	}
}

func TestAnalyzeRejections(t *testing.T) {
	game := synth(12, box{}, box{})
	cases := map[string]faceFinder{
		"game character in the centre": fixedFace(face{345, 90, 26}, always),
		"large cutscene face":          fixedFace(face{100, 180, 140}, always),
		"face in few frames":           fixedFace(face{58, 213, 48}, func(i int) bool { return i < 3 }),
		"no face":                      fixedFace(face{}, func(int) bool { return false }),
	}
	for name, finder := range cases {
		t.Run(name, func(t *testing.T) {
			if res := analyze(game, finder, aspect); res.ok {
				t.Fatalf("accepted: %+v", res)
			}
		})
	}
}

func TestClusterFacesPicksTheStableOne(t *testing.T) {
	stable := face{100, 200, 40}
	perFrame := [][]face{
		{stable, {400, 100, 30}}, {{402, 150, 30}}, {stable}, {{101, 201, 41}}, {{300, 300, 20}}, {stable},
	}
	f, n := clusterFaces(perFrame)
	if n != 4 || absInt(f.x-100) > 1 || absInt(f.y-200) > 1 {
		t.Fatalf("cluster = %+v in %d frames", f, n)
	}
}

func TestDetect(t *testing.T) {
	cam := box{496, 162, 636, 266}
	frames := synth(12, cam, box{530, 185, 575, 266})
	var raw []byte
	for _, f := range frames {
		raw = append(raw, f...)
	}
	var gotArgs []string
	d := New(Config{}, WithLogger(slog.New(slog.DiscardHandler)), withFaceFinder(fixedFace(face{551, 205, 44}, always)),
		withRunner(func(_ context.Context, args ...string) ([]byte, error) {
			gotArgs = args
			return raw, nil
		}))
	r, ok, err := d.Detect(context.Background(), "/pending/x.ts")
	if err != nil || !ok || !near(r.X, 496.0/frameW) {
		t.Fatalf("detect = %+v %v %v", r, ok, err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"-skip_frame nokey", "-i /pending/x.ts", "scale=640:360,format=gray", "-frames:v 12", "-threads 2", "-f rawvideo pipe:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	// Too few keyframes: no decision, no error.
	d = New(Config{}, withFaceFinder(fixedFace(face{}, always)), withRunner(func(context.Context, ...string) ([]byte, error) {
		return raw[:2*frameW*frameH], nil
	}))
	if _, ok, err := d.Detect(context.Background(), "x"); ok || err != nil {
		t.Fatalf("two frames: %v %v", ok, err)
	}
	// ffmpeg failure is an error; a cancelled context too.
	d = New(Config{}, withFaceFinder(fixedFace(face{}, always)), withRunner(func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("invalid data")
	}))
	if _, _, err := d.Detect(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "invalid data") {
		t.Fatalf("ffmpeg error: %v", err)
	}
	d.sem <- struct{}{} // busy
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.Detect(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestPigoFinderOnAFlatFrameFindsNothing(t *testing.T) {
	if faces := pigoFinder()(make([]uint8, frameW*frameH)); len(faces) != 0 {
		t.Fatalf("faces on a flat frame: %v", faces)
	}
}

func TestNormalizeClamps(t *testing.T) {
	r := normalize(box{-10, -5, frameW + 20, 180})
	if r != (domain.Rect{X: 0, Y: 0, W: 1, H: 0.5}) {
		t.Fatalf("normalize = %+v", r)
	}
}
