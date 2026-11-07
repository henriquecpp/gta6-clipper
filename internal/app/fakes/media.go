package fakes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// RetainCall records one RetainWindow call.
type RetainCall struct {
	StreamID domain.StreamID
	From, To domain.StreamPos
}

// MediaCapture is an in-memory MediaCapture that tracks live refs.
type MediaCapture struct {
	mu        sync.Mutex
	Calls     []RetainCall
	Live      map[domain.MediaRef]bool
	Deleted   []domain.MediaRef
	RetainErr error
	DeleteErr error
	// Pad widens the retained window on both sides.
	Pad domain.StreamPos
}

var _ app.MediaCapture = (*MediaCapture)(nil)

// NewMediaCapture returns an empty fake.
func NewMediaCapture() *MediaCapture {
	return &MediaCapture{Live: make(map[domain.MediaRef]bool)}
}

// RetainWindow records the call and returns a ref; the retained window is the
// requested one widened by Pad on each side (like whole segments would).
func (m *MediaCapture) RetainWindow(ctx context.Context, id domain.StreamID, from, to domain.StreamPos) (domain.MediaRef, domain.Window, error) {
	if err := ctx.Err(); err != nil {
		return "", domain.Window{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.RetainErr != nil {
		return "", domain.Window{}, m.RetainErr
	}
	m.Calls = append(m.Calls, RetainCall{StreamID: id, From: from, To: to})
	ref := domain.MediaRef(fmt.Sprintf("/pending/%s-%d.ts", id, len(m.Calls)))
	m.Live[ref] = true
	return ref, domain.Window{From: max(0, from-m.Pad), To: to + m.Pad}, nil
}

// Delete removes a ref.
func (m *MediaCapture) Delete(_ context.Context, ref domain.MediaRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DeleteErr != nil {
		return m.DeleteErr
	}
	delete(m.Live, ref)
	m.Deleted = append(m.Deleted, ref)
	return nil
}

// Add registers a ref as existing (e.g. a preview made by a renderer fake).
func (m *MediaCapture) Add(ref domain.MediaRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Live[ref] = true
}

// IsLive reports whether ref exists.
func (m *MediaCapture) IsLive(ref domain.MediaRef) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Live[ref]
}

// Metrics counts what the use cases report.
type Metrics struct {
	mu       sync.Mutex
	Moments  map[string]int
	States   map[domain.ClipState]int
	Stages   map[string]int
	Latency  []time.Duration
	Cost     domain.MicroUSD
	Usage    domain.Usage
	Approved map[string]int
	Decided  map[string]int
	Queues   map[string]int
	Z        map[string]float64
}

var _ app.Metrics = (*Metrics)(nil)

// NewMetrics returns an empty recorder.
func NewMetrics() *Metrics {
	return &Metrics{
		Moments: map[string]int{}, States: map[domain.ClipState]int{}, Stages: map[string]int{},
		Approved: map[string]int{}, Decided: map[string]int{}, Queues: map[string]int{}, Z: map[string]float64{},
	}
}

// MomentDetected implements app.Metrics.
func (m *Metrics) MomentDetected(s string) { m.mu.Lock(); m.Moments[s]++; m.mu.Unlock() }

// SignalZ implements app.Metrics.
func (m *Metrics) SignalZ(s string, k domain.SignalKind, z float64) {
	m.mu.Lock()
	m.Z[s+"/"+string(k)] = z
	m.mu.Unlock()
}

// ClipState implements app.Metrics.
func (m *Metrics) ClipState(st domain.ClipState) { m.mu.Lock(); m.States[st]++; m.mu.Unlock() }

// StageDuration implements app.Metrics.
func (m *Metrics) StageDuration(stage string, _ time.Duration) {
	m.mu.Lock()
	m.Stages[stage]++
	m.mu.Unlock()
}

// PreviewLatency implements app.Metrics.
func (m *Metrics) PreviewLatency(d time.Duration) {
	m.mu.Lock()
	m.Latency = append(m.Latency, d)
	m.mu.Unlock()
}

// EditorUsage implements app.Metrics.
func (m *Metrics) EditorUsage(c domain.MicroUSD, u domain.Usage) {
	m.mu.Lock()
	m.Cost += c
	m.Usage = m.Usage.Add(u)
	m.mu.Unlock()
}

// Decision implements app.Metrics.
func (m *Metrics) Decision(s string, approved bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Decided[s]++
	if approved {
		m.Approved[s]++
	}
}

// QueueDepth implements app.Metrics.
func (m *Metrics) QueueDepth(q string, n int) { m.mu.Lock(); m.Queues[q] = n; m.mu.Unlock() }

// StateCount returns how many transitions into st were reported.
func (m *Metrics) StateCount(st domain.ClipState) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.States[st]
}
