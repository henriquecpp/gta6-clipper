// Package capture implements the media capture: Streamlink → ffmpeg producing
// short MPEG-TS segments in a circular buffer plus 16 kHz mono PCM for the
// voice signal, and RetainWindow, which copies a window of the buffer to the
// pending storage without re-encoding. It implements app.MediaCapture and the
// Capturer interface of the twitchingest supervisor.
package capture

import (
	"math"
	"slices"
)

// biquad is a second-order IIR filter (RBJ audio EQ cookbook), direct form I.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func newBiquad(highPass bool, f0, fs, q float64) biquad {
	w0 := 2 * math.Pi * f0 / fs
	cos, alpha := math.Cos(w0), math.Sin(w0)/(2*q)
	a0 := 1 + alpha
	var b0, b1, b2 float64
	if highPass {
		b0, b1, b2 = (1+cos)/2, -(1 + cos), (1+cos)/2
	} else {
		b0, b1, b2 = (1-cos)/2, 1-cos, (1-cos)/2
	}
	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: -2 * cos / a0, a2: (1 - alpha) / a0}
}

func (f *biquad) process(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// VAD thresholds (docs/decisions/0018).
const (
	vadMarginDB     = 6.0   // speech must exceed the noise floor by this much
	vadMinDBFS      = -50.0 // absolute floor: quieter frames are never speech
	vadMinBandRatio = 0.50  // share of energy in the voice band
	vadMinZCR       = 0.005 // zero crossings per sample
	vadMaxZCR       = 0.45
	vadFloorWindowS = 10 // noise floor = minimum frame level over this many seconds
	vadHangoverMS   = 240
	silenceDBFS     = -100.0
	vadSilenceGate  = -90.0 // frames below this are digital silence
	int16FullScale  = 32768.0
	voiceBandLowHz  = 120.0
	voiceBandHighHz = 4000.0
	butterworthQ    = 0.7071
)

// VAD is a lightweight pure-Go voice activity detector: a frame is speech when
// its energy is well above the adaptive noise floor, most of it lies in the
// voice band (120–4000 Hz) and its zero-crossing rate is in the range of speech. A short
// hangover bridges the gaps between syllables.
type VAD struct {
	hp, lp        biquad
	prevSign      bool
	framesPerSec  int
	minRing       []float64 // per-second minimum frame level
	ringPos       int
	frameInSec    int
	secLevels     []float64
	filled        int
	hangover      int
	hangoverLimit int
}

// NewVAD builds a detector for the sample rate and frame duration (in ms).
func NewVAD(sampleRate, frameMS int) *VAD {
	fps := max(1000/max(frameMS, 1), 1)
	return &VAD{
		hp:            newBiquad(true, voiceBandLowHz, float64(sampleRate), butterworthQ),
		lp:            newBiquad(false, voiceBandHighHz, float64(sampleRate), butterworthQ),
		framesPerSec:  fps,
		minRing:       make([]float64, vadFloorWindowS),
		hangoverLimit: max(vadHangoverMS/max(frameMS, 1), 1),
	}
}

// Frame analyses one frame of 16-bit PCM and returns its RMS level in dBFS
// and whether it is speech.
func (v *VAD) Frame(samples []int16) (dbfs float64, speech bool) {
	if len(samples) == 0 {
		return silenceDBFS, false
	}
	var sumSq, bandSq float64
	crossings := 0
	for _, s := range samples {
		x := float64(s) / int16FullScale
		sumSq += x * x
		y := v.lp.process(v.hp.process(x))
		bandSq += y * y
		sign := x >= 0
		if sign != v.prevSign {
			crossings++
		}
		v.prevSign = sign
	}
	n := float64(len(samples))
	rms := math.Sqrt(sumSq / n)
	dbfs = silenceDBFS
	if rms > 0 {
		dbfs = max(20*math.Log10(rms), silenceDBFS)
	}
	ratio := 0.0
	if sumSq > 0 {
		ratio = bandSq / sumSq
	}
	zcr := float64(crossings) / n

	floor := v.updateFloor(dbfs)
	active := dbfs > vadMinDBFS && dbfs > floor+vadMarginDB &&
		ratio > vadMinBandRatio && zcr > vadMinZCR && zcr < vadMaxZCR
	if active {
		v.hangover = v.hangoverLimit
		return dbfs, true
	}
	if v.hangover > 0 && dbfs > vadMinDBFS {
		v.hangover--
		return dbfs, true
	}
	v.hangover = 0
	return dbfs, false
}

// updateFloor estimates the background level with minimum statistics: the
// floor is the lowest per-second low percentile over the last seconds, so the
// gaps between words reveal the background even during long monologues over
// game audio, while a single quiet frame (codec start, glitch) cannot drag
// it down. Digital silence (muted audio) is not background noise and is
// ignored.
func (v *VAD) updateFloor(dbfs float64) float64 {
	if dbfs > vadSilenceGate {
		v.secLevels = append(v.secLevels, dbfs)
	}
	v.frameInSec++
	if v.frameInSec >= v.framesPerSec {
		if len(v.secLevels) > 0 {
			v.minRing[v.ringPos] = lowPercentile(v.secLevels)
			v.ringPos = (v.ringPos + 1) % len(v.minRing)
			v.filled = min(v.filled+1, len(v.minRing))
		}
		v.frameInSec, v.secLevels = 0, v.secLevels[:0]
	}
	ok := false
	floor := 0.0
	if v.filled == 0 && len(v.secLevels) > 0 {
		floor, ok = lowPercentile(v.secLevels), true // start-up: current second only
	}
	for i := 0; i < v.filled; i++ {
		if !ok || v.minRing[i] < floor {
			floor, ok = v.minRing[i], true
		}
	}
	if !ok {
		return vadSilenceGate
	}
	return floor
}

// lowPercentile returns the 10th percentile of levels (copying, not sorting in place).
func lowPercentile(levels []float64) float64 {
	tmp := slices.Clone(levels)
	slices.Sort(tmp)
	return tmp[len(tmp)/10]
}
