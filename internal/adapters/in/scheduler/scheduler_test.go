package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestSchedulerRunsTasksPeriodically(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fast, slow, failing atomic.Int32
		s := New(slog.New(slog.DiscardHandler),
			Task{Name: "fast", Interval: time.Minute, Immediate: true, Run: func(context.Context) error { fast.Add(1); return nil }},
			Task{Name: "slow", Interval: time.Hour, Run: func(context.Context) error { slow.Add(1); return nil }},
			Task{Name: "failing", Interval: 30 * time.Second, Run: func(context.Context) error {
				failing.Add(1)
				return errors.New("boom")
			}},
			Task{Name: "invalid", Interval: 0, Run: func(context.Context) error { return nil }},
		)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		if got := fast.Load(); got != 4 { // immediate + 3 ticks
			t.Errorf("fast ran %d times, want 4", got)
		}
		if got := slow.Load(); got != 0 {
			t.Errorf("slow ran %d times, want 0", got)
		}
		if got := failing.Load(); got != 6 {
			t.Errorf("failing ran %d times, want 6 (errors are not fatal)", got)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestJitterBounds(t *testing.T) {
	s := New(slog.New(slog.DiscardHandler))
	task := Task{Interval: 100 * time.Second, Jitter: 0.1}
	for _, r := range []float64{0, 0.5, 0.999} {
		s.rand = func() float64 { return r }
		d := s.next(task)
		if d < 90*time.Second || d > 110*time.Second {
			t.Errorf("rand=%v next=%v outside ±10%%", r, d)
		}
	}
	if s.next(Task{Interval: time.Second}) != time.Second {
		t.Error("no jitter should return the interval")
	}
	s.rand = func() float64 { return 1 }
	if d := s.next(Task{Interval: 10 * time.Second, Jitter: 5}); d != 20*time.Second {
		t.Errorf("jitter must be clamped to 1: %v", d)
	}
}
