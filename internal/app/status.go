package app

import (
	"context"
	"fmt"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// StatusReport is what /status shows.
type StatusReport struct {
	Lives       []domain.LiveStream
	Pending     int
	Queue       int
	LastError   string
	LastErrorAt time.Time
	// CostToday is the editor cost of clips edited since 00:00 UTC.
	CostToday  domain.MicroUSD
	ClipsToday map[domain.ClipState]int
}

// PendingClip is one entry of /pending.
type PendingClip struct {
	Clip     *domain.Clip
	Deadline time.Time
}

// Status answers the /status and /pending commands.
type Status struct {
	repo   ClipRepository
	clock  Clock
	ttl    time.Duration
	lives  func() []domain.LiveStream
	queue  func() int
	errors *ErrorLog
}

// NewStatus builds the use case; lives and queue may be nil.
func NewStatus(repo ClipRepository, clock Clock, ttl time.Duration, lives func() []domain.LiveStream, queue func() int, errs *ErrorLog) *Status {
	if errs == nil {
		errs = &ErrorLog{}
	}
	return &Status{repo: repo, clock: clock, ttl: ttl, lives: lives, queue: queue, errors: errs}
}

// SetQueue sets the queue depth source (wired after the producer exists).
func (s *Status) SetQueue(f func() int) { s.queue = f }

// Report builds the status report.
func (s *Status) Report(ctx context.Context) (StatusReport, error) {
	r := StatusReport{ClipsToday: map[domain.ClipState]int{}}
	if s.lives != nil {
		r.Lives = s.lives()
	}
	if s.queue != nil {
		r.Queue = s.queue()
	}
	r.LastError, r.LastErrorAt = s.errors.Last()
	now := s.clock.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	clips, err := s.repo.ListByState(ctx, domain.ClipStates...)
	if err != nil {
		return r, fmt.Errorf("status: %w", err)
	}
	for _, c := range clips {
		if c.State == domain.StatePreviewSent {
			r.Pending++
		}
		if !c.CreatedAt.Before(midnight) {
			r.ClipsToday[c.State]++
		}
		if at, ok := c.EnteredAt(domain.StateEditing); ok && !at.Before(midnight) {
			r.CostToday += c.Cost
		}
	}
	return r, nil
}

// Pending lists the clips awaiting a decision, oldest first.
func (s *Status) Pending(ctx context.Context) ([]PendingClip, error) {
	clips, err := s.repo.ListByState(ctx, domain.StatePreviewSent)
	if err != nil {
		return nil, fmt.Errorf("pending: %w", err)
	}
	out := make([]PendingClip, 0, len(clips))
	for _, c := range clips {
		deadline, _ := c.ApprovalDeadline(s.ttl)
		out = append(out, PendingClip{Clip: c, Deadline: deadline})
	}
	return out, nil
}
