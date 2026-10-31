// Package scheduler runs periodic tasks (live detection, token validation,
// approval expiration, ...). Each task runs in its own goroutine owned by Run,
// with jitter to avoid bursts, and stops when the context is cancelled.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"golang.org/x/sync/errgroup"
)

// Task is a periodic job.
type Task struct {
	Name     string
	Interval time.Duration
	// Jitter is the fraction of Interval randomly added or removed (0–1).
	Jitter float64
	// Immediate runs the task once at start, before the first interval.
	Immediate bool
	Run       func(ctx context.Context) error
}

// Scheduler owns the task goroutines.
type Scheduler struct {
	log   *slog.Logger
	tasks []Task
	rand  func() float64
}

// New builds a scheduler.
func New(log *slog.Logger, tasks ...Task) *Scheduler {
	return &Scheduler{log: log, tasks: tasks, rand: rand.Float64}
}

// Run blocks until ctx is cancelled. Task errors are logged, never fatal.
func (s *Scheduler) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, t := range s.tasks {
		if t.Interval <= 0 || t.Run == nil {
			s.log.Error("scheduler task ignored: invalid interval or nil func", slog.String("stage", t.Name))
			continue
		}
		g.Go(func() error {
			s.loop(ctx, t)
			return nil
		})
	}
	return g.Wait()
}

func (s *Scheduler) loop(ctx context.Context, t Task) {
	if t.Immediate {
		s.runOnce(ctx, t)
	}
	timer := time.NewTimer(s.next(t))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runOnce(ctx, t)
			timer.Reset(s.next(t))
		}
	}
}

func (s *Scheduler) next(t Task) time.Duration {
	j := min(max(t.Jitter, 0), 1)
	if j == 0 {
		return t.Interval
	}
	delta := (s.rand()*2 - 1) * j * float64(t.Interval)
	return t.Interval + time.Duration(delta)
}

func (s *Scheduler) runOnce(ctx context.Context, t Task) {
	if ctx.Err() != nil {
		return
	}
	start := time.Now()
	err := t.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.log.Warn("scheduled task failed", slog.String("stage", t.Name),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.String("err", err.Error()))
		return
	}
	s.log.Debug("scheduled task done", slog.String("stage", t.Name), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
}
