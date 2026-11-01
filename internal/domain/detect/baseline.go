// Package detect holds the pure detection logic: adaptive baselines,
// per-signal detectors (Strategy pattern), the fusion scorer with hysteresis,
// warm-up, cooldown and deduplication of close peaks (CLAUDE.md §9).
package detect

import "math"

// EWMA is an exponentially weighted moving mean and variance, used as the
// adaptive per-streamer baseline of a signal.
type EWMA struct {
	alpha    float64
	minStd   float64
	mean     float64
	variance float64
	n        int
}

// NewEWMA builds a baseline with smoothing factor alpha in (0, 1) and a floor
// for the standard deviation, which keeps z-scores bounded on flat signals.
func NewEWMA(alpha, minStd float64) *EWMA {
	if !(alpha > 0 && alpha < 1) {
		alpha = 0.01
	}
	if !(minStd > 0) || math.IsInf(minStd, 0) {
		minStd = 1e-6
	}
	return &EWMA{alpha: alpha, minStd: minStd}
}

// Z returns the z-score of x against the current baseline without updating
// it. It is 0 until two samples have been seen, and never NaN or infinite.
func (e *EWMA) Z(x float64) float64 {
	if e.n < 2 || !finite(x) {
		return 0
	}
	z := (x - e.mean) / e.Std()
	if !finite(z) {
		return 0
	}
	return z
}

// Update folds x into the baseline. NaN and infinite values are ignored.
func (e *EWMA) Update(x float64) {
	if !finite(x) {
		return
	}
	if e.n == 0 {
		e.mean, e.variance, e.n = x, 0, 1
		return
	}
	// Incremental exponentially weighted mean and variance (West, 1979).
	diff := x - e.mean
	incr := e.alpha * diff
	mean := e.mean + incr
	variance := (1 - e.alpha) * (e.variance + diff*incr)
	if !finite(mean) || !finite(variance) {
		return
	}
	e.mean = mean
	e.variance = max(variance, 0)
	if e.n < math.MaxInt32 {
		e.n++
	}
}

// winsorK bounds how far a single sample can pull the baseline, in standard
// deviations: a spike scores fully but barely moves the mean and variance, so
// a sustained shout keeps a high z-score instead of inflating its own baseline.
const winsorK = 3

// winsorMin is how many samples are folded unclipped before winsorizing.
const winsorMin = 10

// Observe returns the z-score of x and then updates the baseline with x
// winsorized to mean ± 3σ (after the first samples).
func (e *EWMA) Observe(x float64) float64 {
	z := e.Z(x)
	if e.n >= winsorMin && finite(x) {
		lim := winsorK * e.Std()
		x = min(max(x, e.mean-lim), e.mean+lim)
	}
	e.Update(x)
	return z
}

// Mean is the current baseline mean.
func (e *EWMA) Mean() float64 { return e.mean }

// Std is the current standard deviation, floored at minStd.
func (e *EWMA) Std() float64 { return max(math.Sqrt(max(e.variance, 0)), e.minStd) }

// Variance is the raw current variance (never negative).
func (e *EWMA) Variance() float64 { return e.variance }

// Count is the number of samples folded in (saturating).
func (e *EWMA) Count() int { return e.n }

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func clamp(v, lo, hi float64) float64 {
	if !finite(v) {
		return lo
	}
	return min(max(v, lo), hi)
}
