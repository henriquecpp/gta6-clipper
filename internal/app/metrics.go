package app

import (
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Metrics receives the business metrics of the use cases (CLAUDE.md §16).
// The Prometheus implementation lives in platform/metrics and is wired in cmd.
type Metrics interface {
	MomentDetected(streamer string)
	SignalZ(streamer string, kind domain.SignalKind, z float64)
	ClipState(state domain.ClipState)
	StageDuration(stage string, d time.Duration)
	PreviewLatency(d time.Duration)
	EditorUsage(cost domain.MicroUSD, u domain.Usage)
	Decision(streamer string, approved bool)
	QueueDepth(queue string, n int)
}

// NopMetrics discards every metric.
type NopMetrics struct{}

var _ Metrics = NopMetrics{}

// MomentDetected implements Metrics.
func (NopMetrics) MomentDetected(string) {}

// SignalZ implements Metrics.
func (NopMetrics) SignalZ(string, domain.SignalKind, float64) {}

// ClipState implements Metrics.
func (NopMetrics) ClipState(domain.ClipState) {}

// StageDuration implements Metrics.
func (NopMetrics) StageDuration(string, time.Duration) {}

// PreviewLatency implements Metrics.
func (NopMetrics) PreviewLatency(time.Duration) {}

// EditorUsage implements Metrics.
func (NopMetrics) EditorUsage(domain.MicroUSD, domain.Usage) {}

// Decision implements Metrics.
func (NopMetrics) Decision(string, bool) {}

// QueueDepth implements Metrics.
func (NopMetrics) QueueDepth(string, int) {}
