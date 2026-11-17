// Package resilience decorates the outbound ports with retry/backoff, error
// metrics and logs (Decorator pattern, CLAUDE.md §5), so the adapters stay
// free of that plumbing.
package resilience

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Policy is a retry policy with exponential backoff and jitter.
type Policy struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
	// Jitter is the random fraction (0–1) added to or removed from each delay.
	Jitter float64
}

// Delay returns the wait before attempt n (1-based retry number).
func (p Policy) Delay(n int, rnd func() float64) time.Duration {
	d := p.Base
	for i := 1; i < n && d < p.Max; i++ {
		d *= 2
	}
	d = min(d, p.Max)
	if p.Jitter > 0 && rnd != nil {
		d += time.Duration((rnd()*2 - 1) * p.Jitter * float64(d))
	}
	return max(d, 0)
}

// temporary is implemented by adapter errors that know whether a retry helps.
type temporary interface{ Temporary() bool }

// Retryable reports whether an error is worth retrying.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, domain.ErrInvalidEditPlan), errors.Is(err, domain.ErrNotFound),
		errors.Is(err, domain.ErrWindowUnavailable), errors.Is(err, domain.ErrInvalidArgument):
		return false
	}
	var t temporary
	if errors.As(err, &t) {
		return t.Temporary()
	}
	return true
}

// Metrics receives the error count of external ports.
type Metrics interface {
	ExternalError(port string)
}

// Deps are shared by every decorator.
type Deps struct {
	Log     *slog.Logger
	Metrics Metrics
	// rand and sleep are replaceable in tests.
	rand  func() float64
	sleep func(ctx context.Context, d time.Duration) error
}

func (d Deps) withDefaults() Deps {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.rand == nil {
		d.rand = rand.Float64
	}
	if d.sleep == nil {
		d.sleep = func(ctx context.Context, wait time.Duration) error {
			t := time.NewTimer(wait)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return d
}

// call runs op with the policy, counting and logging failures.
func call[T any](ctx context.Context, deps Deps, p Policy, port, op string, fn func(context.Context) (T, error)) (T, error) {
	attempts := max(p.Attempts, 1)
	var zero T
	for attempt := 1; ; attempt++ {
		v, err := fn(ctx)
		if err == nil {
			return v, nil
		}
		if deps.Metrics != nil {
			deps.Metrics.ExternalError(port)
		}
		if attempt >= attempts || !Retryable(err) || ctx.Err() != nil {
			deps.Log.Error("external call failed", slog.String("port", port), slog.String("op", op),
				slog.Int("attempt", attempt), slog.String("err", err.Error()))
			return zero, err
		}
		wait := p.Delay(attempt, deps.rand)
		deps.Log.Warn("external call failed; retrying", slog.String("port", port), slog.String("op", op),
			slog.Int("attempt", attempt), slog.Duration("backoff", wait), slog.String("err", err.Error()))
		if serr := deps.sleep(ctx, wait); serr != nil {
			return zero, errors.Join(err, serr)
		}
	}
}

// call0 is call for operations without a result.
func call0(ctx context.Context, deps Deps, p Policy, port, op string, fn func(context.Context) error) error {
	_, err := call(ctx, deps, p, port, op, func(ctx context.Context) (struct{}, error) { return struct{}{}, fn(ctx) })
	return err
}
