// Package clock provides the real wall clock used by the composition root.
// Domain and app code never call time.Now directly; they receive a Clock.
package clock

import "time"

// Real is the system clock, always in UTC.
type Real struct{}

// Now returns the current time in UTC.
func (Real) Now() time.Time { return time.Now().UTC() }
