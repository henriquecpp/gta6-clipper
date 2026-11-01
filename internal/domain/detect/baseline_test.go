package detect

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestEWMAConvergesAndScores(t *testing.T) {
	e := NewEWMA(0.05, 0.1)
	if e.Z(100) != 0 {
		t.Fatal("z must be 0 without history")
	}
	for i := range 2000 {
		x := 10.0
		if i%2 == 0 {
			x = 12
		}
		e.Update(x)
	}
	if m := e.Mean(); math.Abs(m-11) > 0.1 {
		t.Fatalf("mean = %v, want ~11", m)
	}
	if s := e.Std(); math.Abs(s-1) > 0.1 {
		t.Fatalf("std = %v, want ~1", s)
	}
	if z := e.Z(15); z < 3.5 || z > 4.5 {
		t.Fatalf("z(15) = %v, want ~4", z)
	}
	if z := e.Observe(11); math.Abs(z) > 0.2 {
		t.Fatalf("z(mean) = %v", z)
	}
	if e.Count() != 2001 {
		t.Fatalf("count = %d", e.Count())
	}
}

func TestEWMAIgnoresNonFinite(t *testing.T) {
	e := NewEWMA(0.1, 0.5)
	e.Update(1)
	e.Update(math.NaN())
	e.Update(math.Inf(1))
	if e.Count() != 1 || e.Mean() != 1 {
		t.Fatalf("non-finite values were folded: n=%d mean=%v", e.Count(), e.Mean())
	}
	e.Update(1)
	if z := e.Z(math.NaN()); z != 0 {
		t.Fatalf("Z(NaN) = %v", z)
	}
	if e.Std() != 0.5 {
		t.Fatalf("std floor not applied: %v", e.Std())
	}
}

func TestEWMADefaults(t *testing.T) {
	e := NewEWMA(0, math.NaN())
	if e.alpha != 0.01 || e.minStd != 1e-6 {
		t.Fatalf("defaults: alpha=%v minStd=%v", e.alpha, e.minStd)
	}
	if NewEWMA(2, math.Inf(1)).alpha != 0.01 {
		t.Fatal("alpha >= 1 must fall back")
	}
}

// FuzzEWMA checks that the baseline never produces NaN, infinities or a
// negative variance, whatever the input (CLAUDE.md §17).
func FuzzEWMA(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 240, 63}, 0.01)
	f.Add([]byte{255, 255, 255, 255, 255, 255, 239, 127, 1, 0, 0, 0, 0, 0, 0, 128}, 0.5)
	f.Fuzz(func(t *testing.T, data []byte, alpha float64) {
		e := NewEWMA(alpha, 1e-3)
		for len(data) >= 8 {
			x := math.Float64frombits(binary.LittleEndian.Uint64(data[:8]))
			data = data[8:]
			z := e.Observe(x)
			if math.IsNaN(z) || math.IsInf(z, 0) {
				t.Fatalf("z = %v for x = %v", z, x)
			}
			if e.Variance() < 0 || math.IsNaN(e.Variance()) || math.IsInf(e.Variance(), 0) {
				t.Fatalf("variance = %v", e.Variance())
			}
			if math.IsNaN(e.Mean()) || math.IsInf(e.Mean(), 0) {
				t.Fatalf("mean = %v", e.Mean())
			}
			if e.Std() <= 0 {
				t.Fatalf("std = %v", e.Std())
			}
		}
	})
}

func TestClampAndSecond(t *testing.T) {
	if clamp(math.NaN(), 0, 6) != 0 || clamp(-1, 0, 6) != 0 || clamp(9, 0, 6) != 6 || clamp(3, 0, 6) != 3 {
		t.Fatal("clamp")
	}
	cases := map[int64]int64{0: 0, 999_999_999: 0, 1_000_000_000: 1, -1: -1, -1_000_000_000: -1, -1_000_000_001: -2}
	for ns, want := range cases {
		if got := second(posNS(ns)); got != want {
			t.Errorf("second(%d ns) = %d, want %d", ns, got, want)
		}
	}
}

func TestEWMAWinsorizesSpikes(t *testing.T) {
	e := NewEWMA(0.02, 1)
	for i := range 500 {
		e.Observe(float64(i % 2))
	}
	before := e.Mean()
	first := e.Observe(50)
	for range 4 {
		e.Observe(50)
	}
	last := e.Observe(50)
	if first < 40 || last < 0.8*first {
		t.Fatalf("sustained spike lost its score: first z=%v last z=%v", first, last)
	}
	if e.Mean()-before > 1 {
		t.Fatalf("spike moved the mean too much: %v → %v", before, e.Mean())
	}
}
