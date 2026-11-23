package facecam

import (
	"math"
	"sort"

	pigo "github.com/esimov/pigo/pkg"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Analysis frames are this size (the extraction scales to it).
const frameW, frameH = 640, 360

// face is a face detection in pixels: centre and size.
type face struct{ x, y, s int }

// faceFinder returns the faces of one gray frame (replaced in tests).
type faceFinder func(pix []uint8) []face

// pigoFinder detects frontal faces with pigo's embedded cascade.
func pigoFinder() faceFinder {
	classifier := pigo.NewPigo().Unpack()
	return func(pix []uint8) []face {
		dets := classifier.RunCascade(pigo.CascadeParams{
			MinSize: 20, MaxSize: 220, ShiftFactor: 0.1, ScaleFactor: 1.1,
			ImageParams: pigo.ImageParams{Pixels: pix, Rows: frameH, Cols: frameW, Dim: frameW},
		}, 0)
		var out []face
		for _, d := range classifier.ClusterDetections(dets, 0.2) {
			if d.Q >= 5 {
				out = append(out, face{d.Col, d.Row, d.Scale})
			}
		}
		return out
	}
}

// result is the outcome of analyze.
type result struct {
	rect   domain.Rect
	ok     bool
	reason string
	face   face
	hits   int
	sides  [4]float64 // left, right, top, bottom boundary scores
}

// Thresholds tuned on real GTA VI streams (ADR 0027).
const (
	// strongSide: every side of a camera box must reach this score.
	strongSide = 0.55
	// minHitShare of the frames must show the face at the same place.
	minHitShare = 0.4
	// Faces in the centre band or larger than maxFaceShare of the width are
	// usually game characters: accepted only inside a clear box.
	centerBandLo, centerBandHi = 0.38, 0.62
	maxFaceShare               = 0.16
	// camWidthFaces is the width of a face-proportional crop, in face sizes.
	camWidthFaces = 3.4
	// faceFromTop places the face in a face-proportional crop.
	faceFromTop = 0.42
)

// analyze finds the camera: the face that stays at the same place anchors
// the search; each side of the box is where the inside stays still and the
// outside moves (the game) along a line that is an edge in most frames. A
// camera without a clear box (background removed) gets a crop proportional
// to the face with the given aspect (width/height in output pixels).
func analyze(frames [][]uint8, find faceFinder, aspect float64) result {
	n := len(frames)
	perFrame := make([][]face, n)
	for i, f := range frames {
		perFrame[i] = find(f)
	}
	f, hits := clusterFaces(perFrame)
	res := result{face: f, hits: hits}
	if hits == 0 || float64(hits) < math.Max(2, minHitShare*float64(n)) {
		res.reason = "no face stays at the same place"
		return res
	}
	fx, fsz := float64(f.x)/frameW, float64(f.s)/frameW
	if fsz > maxFaceShare {
		res.reason = "face too large for a camera (game cutscene)"
		return res
	}
	sd := stdMap(frames)
	px, py := persistence(frames)
	b, sides := fitBox(sd, px, py, f)
	res.sides = sides
	if min(sides[0], sides[1], sides[2], sides[3]) >= strongSide && plausibleBox(b) {
		res.rect, res.ok, res.reason = normalize(b), true, "camera box"
		return res
	}
	if fx > centerBandLo && fx < centerBandHi {
		res.reason = "face looks like game content and has no camera box"
		return res
	}
	res.rect, res.ok, res.reason = normalize(proportional(f, aspect)), true, "face-proportional crop"
	return res
}

// clusterFaces keeps the face found at the same place in the most frames
// (median position and size); a face in a moving game does not stay put.
func clusterFaces(perFrame [][]face) (face, int) {
	var best face
	bestN := 0
	for _, frame := range perFrame {
		for _, c := range frame {
			var xs, ys, ss []int
			for _, other := range perFrame {
				for _, o := range other {
					if absInt(o.x-c.x) < c.s/2 && absInt(o.y-c.y) < c.s/2 {
						xs, ys, ss = append(xs, o.x), append(ys, o.y), append(ss, o.s)
						break
					}
				}
			}
			if len(xs) > bestN {
				bestN = len(xs)
				best = face{median(xs), median(ys), median(ss)}
			}
		}
	}
	return best, bestN
}

// stdMap is the temporal std of every pixel: high where the game moves, low
// in the camera's room (even with screen-light flicker).
func stdMap(frames [][]uint8) []float64 {
	n := float64(len(frames))
	sd := make([]float64, frameW*frameH)
	for p := range sd {
		var sum, sq float64
		for _, f := range frames {
			v := float64(f[p])
			sum += v
			sq += v * v
		}
		m := sum / n
		sd[p] = math.Sqrt(math.Max(sq/n-m*m, 0))
	}
	return sd
}

// persistence is the share of frames with a vertical (px) or horizontal (py)
// discontinuity at each pixel: the camera border is one in almost every
// frame, game edges move around.
func persistence(frames [][]uint8) (px, py []float64) {
	px, py = make([]float64, frameW*frameH), make([]float64, frameW*frameH)
	inc := 1 / float64(len(frames))
	for _, f := range frames {
		for y := 1; y < frameH-1; y++ {
			for x := 1; x < frameW-1; x++ {
				p := y*frameW + x
				if absInt(int(f[p+1])-int(f[p-1])) > 18 {
					px[p] += inc
				}
				if absInt(int(f[p+frameW])-int(f[p-frameW])) > 18 {
					py[p] += inc
				}
			}
		}
	}
	return px, py
}

// outsideStd stands for the area beyond the frame edge (like a moving game),
// so the frame edge is a valid boundary.
const outsideStd = 40

func meanStd(sd []float64, x0, y0, x1, y1 int) float64 {
	x0, y0 = max(x0, 0), max(y0, 0)
	x1, y1 = min(x1, frameW), min(y1, frameH)
	if x1 <= x0 || y1 <= y0 {
		return outsideStd
	}
	var s float64
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			s += sd[y*frameW+x]
		}
	}
	return s / float64((x1-x0)*(y1-y0))
}

// stillMoving is 1 when the inside is still and the outside moves.
func stillMoving(in, out float64) float64 { return (out - in) / (out + in + 1) }

// vline is the persistence of a vertical line at x over [y0, y1), with one
// pixel of slack; the frame edge is a full line.
func vline(px []float64, x, y0, y1 int) float64 {
	if x <= 1 || x >= frameW-2 {
		return 1
	}
	y0, y1 = max(y0, 0), min(y1, frameH)
	var s float64
	for y := y0; y < y1; y++ {
		s += max(px[y*frameW+x-1], px[y*frameW+x], px[y*frameW+x+1])
	}
	return s / float64(max(y1-y0, 1))
}

func hline(py []float64, y, x0, x1 int) float64 {
	if y <= 1 || y >= frameH-2 {
		return 1
	}
	x0, x1 = max(x0, 0), min(x1, frameW)
	var s float64
	for x := x0; x < x1; x++ {
		s += max(py[(y-1)*frameW+x], py[y*frameW+x], py[(y+1)*frameW+x])
	}
	return s / float64(max(x1-x0, 1))
}

// box is a rectangle in pixels (x1, y1 exclusive).
type box struct{ x0, y0, x1, y1 int }

// fitBox looks for each side of the camera box within a range proportional
// to the face; top and bottom use the box's side columns, away from the
// person (who moves).
func fitBox(sd, px, py []float64, f face) (box, [4]float64) {
	cx, cy, fs := f.x, f.y, max(f.s, 8)
	k := max(fs/4, 3) // thickness of the strips compared on each side
	best := func(lo, hi int, score func(int) float64) (int, float64) {
		bv, bs := lo, math.Inf(-1)
		for v := lo; v <= hi; v++ {
			if s := score(v); s > bs {
				bv, bs = v, s
			}
		}
		return bv, bs
	}
	ry0, ry1 := cy-fs/2, cy+fs // from the eyes to the shoulders
	left, sl := best(max(cx-4*fs, 0), max(cx-7*fs/10, 0), func(x int) float64 {
		return (stillMoving(meanStd(sd, x, ry0, x+k, ry1), meanStd(sd, x-k, ry0, x, ry1)) + vline(px, x, ry0-fs/2, ry1)) / 2
	})
	right, sr := best(min(cx+7*fs/10, frameW), min(cx+4*fs, frameW), func(x int) float64 {
		return (stillMoving(meanStd(sd, x-k, ry0, x, ry1), meanStd(sd, x, ry0, x+k, ry1)) + vline(px, x, ry0-fs/2, ry1)) / 2
	})
	w := right - left
	bands := [][2]int{{left + k, left + max(w/5, 2*k)}, {right - max(w/5, 2*k), right - k}}
	rowStd := func(y0, y1 int) float64 {
		var s float64
		for _, b := range bands {
			s += meanStd(sd, b[0], y0, b[1], y1)
		}
		return s / float64(len(bands))
	}
	top, st := best(max(cy-5*fs/2, 0), max(cy-fs/2, 0), func(y int) float64 {
		return (stillMoving(rowStd(y, y+k), rowStd(y-k, y)) + hline(py, y, left, right)) / 2
	})
	// A camera is landscape or about square: the bottom is never searched
	// below top + width/minCamAspect. Without this, the frame's bottom edge
	// (a perfect "line" with "moving" content beyond it) beats a faint real
	// border when the game under the camera moves little.
	bottomMax := min(cy+4*fs, frameH, top+int(float64(w)/minCamAspect))
	bottom, sb := best(min(cy+7*fs/10, frameH), bottomMax, func(y int) float64 {
		return (stillMoving(rowStd(y-k, y), rowStd(y, y+k)) + hline(py, y, left, right)) / 2
	})
	return box{left, top, right, bottom}, [4]float64{sl, sr, st, sb}
}

// maxBoxShare is the largest share of the frame a camera box may cover.
const maxBoxShare = 0.35

// Camera boxes are landscape or about square (width/height in pixels): 4:3
// and 16:9 webcams, taller with an alert bar under them (1.36 for alanzoka,
// 1.6–1.8 for xqc and facada).
const minCamAspect, maxCamAspect = 0.9, 2.4

// plausibleBox rejects "boxes" that are really the frame or a bad fit: a
// camera never touches opposite edges, covers most of the screen, or is
// portrait or a thin strip.
func plausibleBox(b box) bool {
	if (b.x0 <= 1 && b.x1 >= frameW-1) || (b.y0 <= 1 && b.y1 >= frameH-1) {
		return false
	}
	w, h := b.x1-b.x0, b.y1-b.y0
	if w <= 0 || h <= 0 {
		return false
	}
	aspect := float64(w) / float64(h)
	return aspect >= minCamAspect && aspect <= maxCamAspect && float64(w*h) <= maxBoxShare*frameW*frameH
}

// proportional is a crop around the face, camWidthFaces wide, with the face
// at faceFromTop of the height, shifted (not shrunk) to stay in the frame.
// aspect is width/height in output pixels; frame pixels are square.
func proportional(f face, aspect float64) box {
	w := camWidthFaces * float64(f.s)
	h := w / aspect
	x0 := math.Max(0, math.Min(float64(f.x)-w/2, frameW-w))
	y0 := math.Max(0, math.Min(float64(f.y)-faceFromTop*h, frameH-h))
	return box{int(math.Round(x0)), int(math.Round(y0)), int(math.Round(x0 + w)), int(math.Round(y0 + h))}
}

// normalize converts a pixel box to a frame-relative rectangle.
func normalize(b box) domain.Rect {
	x0, y0 := max(b.x0, 0), max(b.y0, 0)
	x1, y1 := min(b.x1, frameW), min(b.y1, frameH)
	r := func(v float64) float64 { return math.Round(v*1000) / 1000 }
	return domain.Rect{
		X: r(float64(x0) / frameW), Y: r(float64(y0) / frameH),
		W: r(float64(x1-x0) / frameW), H: r(float64(y1-y0) / frameH),
	}
}

func median(v []int) int {
	s := append([]int(nil), v...)
	sort.Ints(s)
	return s[len(s)/2]
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
