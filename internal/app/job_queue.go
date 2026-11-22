package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// JobQueue is a worker pool of clip jobs (final production, publication): a
// bounded queue, a fixed number of workers and deduplication of clips already
// queued or running. GPU-bound stages are further limited inside the adapters.
type JobQueue struct {
	name    string
	run     func(ctx context.Context, id domain.ClipID) error
	workers int
	grace   time.Duration
	log     *slog.Logger
	metrics Metrics

	jobs   chan domain.ClipID
	mu     sync.Mutex
	queued map[domain.ClipID]bool
	closed bool
}

// NewJobQueue builds the pool; name labels its logs and the queue depth
// metric. grace is how long running jobs may take to finish after shutdown
// starts.
func NewJobQueue(name string, run func(ctx context.Context, id domain.ClipID) error, workers, capacity int, grace time.Duration, log *slog.Logger, metrics Metrics) *JobQueue {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	return &JobQueue{
		name: name, run: run, workers: max(workers, 1), grace: grace, log: log, metrics: metrics,
		jobs: make(chan domain.ClipID, max(capacity, 1)), queued: make(map[domain.ClipID]bool),
	}
}

// Submit enqueues a clip without blocking; it returns false if the clip is
// already queued or the queue is full/closed (the clip stays in a resumable
// state and is picked up by RecoverPending on the next start).
func (q *JobQueue) Submit(_ context.Context, id domain.ClipID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.queued[id] {
		return false
	}
	select {
	case q.jobs <- id:
		q.queued[id] = true
		q.metrics.QueueDepth(q.name, len(q.jobs))
		return true
	default:
		q.log.Error("job queue full; clip left for recovery", slog.String("queue", q.name), slog.String("clip_id", string(id)))
		return false
	}
}

// Depth returns how many clips are waiting or running.
func (q *JobQueue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queued)
}

// Run processes jobs until ctx is cancelled; running jobs then get the grace
// period to finish (their context is cancelled after it).
func (q *JobQueue) Run(ctx context.Context) error {
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	var wg sync.WaitGroup
	for range q.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-q.jobs:
					q.metrics.QueueDepth(q.name, len(q.jobs))
					if ctx.Err() != nil {
						return // shutting down: the clip stays resumable for RecoverPending
					}
					if err := q.run(jobCtx, id); err != nil {
						q.log.Warn(q.name+" failed", slog.String("clip_id", string(id)), slog.String("err", err.Error()))
					}
					q.mu.Lock()
					delete(q.queued, id)
					q.mu.Unlock()
				}
			}
		}()
	}
	<-ctx.Done()
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(q.grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		q.log.Warn("shutdown grace expired; cancelling running jobs (they resume on restart)", slog.String("queue", q.name))
		cancelJobs()
		<-done
	}
	return nil
}
