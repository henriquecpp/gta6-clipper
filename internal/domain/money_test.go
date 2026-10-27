package domain

import "testing"

func TestUSDToMicro(t *testing.T) {
	cases := map[float64]MicroUSD{0.05: 50_000, 0.1: 100_000, 3: 3_000_000, 0.0000004: 0, 0.0000006: 1}
	for in, want := range cases {
		if got := USDToMicro(in); got != want {
			t.Errorf("USDToMicro(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestMicroUSDString(t *testing.T) {
	cases := map[MicroUSD]string{0: "$0.000000", 12345: "$0.012345", 1_500_000: "$1.500000", -50: "-$0.000050"}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", in, got, want)
		}
	}
}

func TestUsageCost(t *testing.T) {
	// Haiku 5.5-like prices: $0.10 in, $0.50 out, $0.125 cache write, $0.01 cache read per MTok.
	p := Pricing{Input: USDToMicro(0.10), Output: USDToMicro(0.50), CacheWrite: USDToMicro(0.125), CacheRead: USDToMicro(0.01)}
	u := Usage{InputTokens: 6000, OutputTokens: 900, CacheWriteTokens: 2000, CacheReadTokens: 0}
	// 6000*0.1 + 900*0.5 + 2000*0.125 = 600 + 450 + 250 = 1300 µUSD
	if got := u.Cost(p); got != 1300 {
		t.Fatalf("cost = %d, want 1300", got)
	}
	// Rounding to the nearest micro-dollar.
	if got := (Usage{InputTokens: 7}).Cost(Pricing{Input: USDToMicro(0.10)}); got != 1 {
		t.Fatalf("rounded cost = %d, want 1", got)
	}
	sum := u.Add(Usage{InputTokens: 1, OutputTokens: 2, CacheWriteTokens: 3, CacheReadTokens: 4})
	if sum != (Usage{InputTokens: 6001, OutputTokens: 902, CacheWriteTokens: 2003, CacheReadTokens: 4}) {
		t.Fatalf("Add = %+v", sum)
	}
}
