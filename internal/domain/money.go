package domain

import (
	"fmt"
	"math"
)

// MicroUSD is an amount of money in millionths of a US dollar. Money is never
// represented as float.
type MicroUSD int64

// USDToMicro converts a dollar amount (e.g. from configuration) to MicroUSD,
// rounding to the nearest micro-dollar.
func USDToMicro(usd float64) MicroUSD {
	return MicroUSD(math.Round(usd * 1e6))
}

// String formats the amount as dollars with 6 decimals, e.g. "$0.012345".
func (m MicroUSD) String() string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s$%d.%06d", sign, v/1_000_000, v%1_000_000)
}

// Usage is the token accounting of one or more model calls.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheReadTokens  int64
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
	}
}

// Pricing holds prices in MicroUSD per million tokens.
type Pricing struct {
	Input      MicroUSD
	Output     MicroUSD
	CacheWrite MicroUSD
	CacheRead  MicroUSD
}

// Cost computes the price of u, rounding the total to the nearest micro-dollar.
func (u Usage) Cost(p Pricing) MicroUSD {
	// tokens × (µUSD per 1M tokens) / 1M, summed before dividing to keep precision.
	total := u.InputTokens*int64(p.Input) +
		u.OutputTokens*int64(p.Output) +
		u.CacheWriteTokens*int64(p.CacheWrite) +
		u.CacheReadTokens*int64(p.CacheRead)
	return MicroUSD((total + 500_000) / 1_000_000)
}
