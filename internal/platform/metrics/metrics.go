// Package metrics is the Prometheus registry of the clipper (CLAUDE.md §16).
// Prometheus implements app.Metrics (business metrics reported by the use
// cases) plus a few infrastructure gauges set by cmd and the adapters.
package metrics

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Prometheus holds every metric.
type Prometheus struct {
	reg *prometheus.Registry

	streamsLive     prometheus.Gauge
	signalZ         *prometheus.GaugeVec
	momentsDetected *prometheus.CounterVec
	clipsTotal      *prometheus.CounterVec
	stageDuration   *prometheus.HistogramVec
	previewLatency  prometheus.Histogram
	queueDepth      *prometheus.GaugeVec
	editorCost      prometheus.Counter
	editorTokens    *prometheus.CounterVec
	approvalRatio   *prometheus.GaugeVec
	gpuUtilization  prometheus.Gauge
	gpuMemory       prometheus.Gauge
	nvencAvailable  prometheus.Gauge
	externalErrors  *prometheus.CounterVec

	mu       sync.Mutex
	approved map[string]int
	decided  map[string]int
}

// New registers all metrics on a fresh registry (with Go and process collectors).
func New() *Prometheus {
	p := &Prometheus{
		reg:      prometheus.NewRegistry(),
		approved: map[string]int{},
		decided:  map[string]int{},
		streamsLive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "clipper_streams_live", Help: "Live broadcasts being monitored."}),
		signalZ: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clipper_signal_zscore", Help: "Latest clamped value of each detection signal."}, []string{"streamer", "signal"}),
		momentsDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clipper_moments_detected_total", Help: "Moments accepted by the scorer."}, []string{"streamer"}),
		clipsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clipper_clips_total", Help: "Clip transitions into each state."}, []string{"state"}),
		stageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "clipper_stage_duration_seconds", Help: "Duration of each pipeline stage.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}}, []string{"stage"}),
		previewLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "clipper_preview_latency_seconds", Help: "From the peak (wall time) to the preview delivered on Telegram; target < 30 s.",
			Buckets: []float64{20, 30, 40, 45, 50, 60, 75, 90, 120, 180, 300}}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clipper_queue_depth", Help: "Jobs waiting in each queue."}, []string{"queue"}),
		editorCost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "clipper_editor_cost_microusd_total", Help: "Cost of editor (Claude) calls in micro-dollars."}),
		editorTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clipper_editor_tokens_total", Help: "Editor tokens by kind."}, []string{"kind"}),
		approvalRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clipper_approval_ratio", Help: "Approved / decided previews since start."}, []string{"streamer"}),
		gpuUtilization: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "clipper_gpu_utilization", Help: "GPU utilization (0-1) from nvidia-smi."}),
		gpuMemory: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "clipper_gpu_memory_bytes", Help: "GPU memory used, from nvidia-smi."}),
		nvencAvailable: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "clipper_nvenc_available", Help: "1 if h264_nvenc is used, 0 if libx264 is the fallback."}),
		externalErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clipper_external_errors_total", Help: "Errors returned by external ports."}, []string{"port"}),
	}
	p.reg.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		p.streamsLive, p.signalZ, p.momentsDetected, p.clipsTotal, p.stageDuration, p.previewLatency,
		p.queueDepth, p.editorCost, p.editorTokens, p.approvalRatio, p.gpuUtilization, p.gpuMemory,
		p.nvencAvailable, p.externalErrors,
	)
	return p
}

// Handler serves /metrics.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{Registry: p.reg})
}

// Registry exposes the registry (tests).
func (p *Prometheus) Registry() *prometheus.Registry { return p.reg }

// MomentDetected implements app.Metrics.
func (p *Prometheus) MomentDetected(streamer string) {
	p.momentsDetected.WithLabelValues(streamer).Inc()
}

// SignalZ implements app.Metrics.
func (p *Prometheus) SignalZ(streamer string, kind domain.SignalKind, z float64) {
	p.signalZ.WithLabelValues(streamer, string(kind)).Set(z)
}

// ClipState implements app.Metrics.
func (p *Prometheus) ClipState(state domain.ClipState) {
	p.clipsTotal.WithLabelValues(string(state)).Inc()
}

// StageDuration implements app.Metrics.
func (p *Prometheus) StageDuration(stage string, d time.Duration) {
	p.stageDuration.WithLabelValues(stage).Observe(d.Seconds())
}

// PreviewLatency implements app.Metrics.
func (p *Prometheus) PreviewLatency(d time.Duration) { p.previewLatency.Observe(d.Seconds()) }

// EditorUsage implements app.Metrics.
func (p *Prometheus) EditorUsage(cost domain.MicroUSD, u domain.Usage) {
	p.editorCost.Add(float64(cost))
	p.editorTokens.WithLabelValues("input").Add(float64(u.InputTokens))
	p.editorTokens.WithLabelValues("output").Add(float64(u.OutputTokens))
	p.editorTokens.WithLabelValues("cache_write").Add(float64(u.CacheWriteTokens))
	p.editorTokens.WithLabelValues("cache_read").Add(float64(u.CacheReadTokens))
}

// Decision implements app.Metrics.
func (p *Prometheus) Decision(streamer string, approved bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decided[streamer]++
	if approved {
		p.approved[streamer]++
	}
	p.approvalRatio.WithLabelValues(streamer).Set(float64(p.approved[streamer]) / float64(p.decided[streamer]))
}

// QueueDepth implements app.Metrics.
func (p *Prometheus) QueueDepth(queue string, n int) {
	p.queueDepth.WithLabelValues(queue).Set(float64(n))
}

// SetStreamsLive sets the number of monitored lives.
func (p *Prometheus) SetStreamsLive(n int) { p.streamsLive.Set(float64(n)) }

// SetNVENC records whether NVENC is in use.
func (p *Prometheus) SetNVENC(ok bool) {
	if ok {
		p.nvencAvailable.Set(1)
	} else {
		p.nvencAvailable.Set(0)
	}
}

// ExternalError counts an error from an external port.
func (p *Prometheus) ExternalError(port string) { p.externalErrors.WithLabelValues(port).Inc() }

// GPUSampler polls nvidia-smi (DCGM may not work on WSL2) and reports when the
// GPU leaves the container and when it comes back. A driver update on Windows
// takes the GPU away from the WSL VM until it restarts (2026-11-25).
type GPUSampler struct {
	p        *Prometheus
	run      func(ctx context.Context) ([]byte, error)
	log      *slog.Logger
	onChange func(ctx context.Context, up bool)
	failures int
	down     bool
}

// gpuDownAfter consecutive failed samples mark the GPU as gone (one failure
// can be a hiccup).
const gpuDownAfter = 2

// OnChange registers f, called once when the GPU becomes unavailable
// (up=false) and once when it comes back.
func (s *GPUSampler) OnChange(f func(ctx context.Context, up bool)) { s.onChange = f }

// failed counts a failed sample and reports whether the error is still news
// (false once the GPU is known to be down, so the log is not flooded).
func (s *GPUSampler) failed(ctx context.Context) bool {
	if s.down {
		return false
	}
	s.failures++
	if s.failures >= gpuDownAfter {
		s.down = true
		if s.onChange != nil {
			s.onChange(ctx, false)
		}
	}
	return true
}

// succeeded resets the failures and reports a recovery.
func (s *GPUSampler) succeeded(ctx context.Context) {
	s.failures = 0
	if s.down {
		s.down = false
		if s.onChange != nil {
			s.onChange(ctx, true)
		}
	}
}

// NewGPUSampler builds a sampler; run executes
// `nvidia-smi --query-gpu=utilization.gpu,memory.used --format=csv,noheader,nounits`.
func (p *Prometheus) NewGPUSampler(run func(ctx context.Context) ([]byte, error), log *slog.Logger) *GPUSampler {
	return &GPUSampler{p: p, run: run, log: log}
}

// Sample reads the GPU once. Samples run one at a time (scheduler task).
func (s *GPUSampler) Sample(ctx context.Context) error {
	out, err := s.run(ctx)
	if err != nil {
		if ctx.Err() != nil || !s.failed(ctx) {
			return nil // shutting down, or already reported as down
		}
		return err
	}
	s.succeeded(ctx)
	util, memMiB, ok := parseNvidiaSMI(out)
	if !ok {
		return nil
	}
	s.p.gpuUtilization.Set(util / 100)
	s.p.gpuMemory.Set(memMiB * 1024 * 1024)
	return nil
}

// parseNvidiaSMI parses the first "util, memMiB" line.
func parseNvidiaSMI(out []byte) (util, memMiB float64, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ",")
		if len(parts) < 2 {
			continue
		}
		u, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		m, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 == nil && err2 == nil {
			return u, m, true
		}
	}
	return 0, 0, false
}
