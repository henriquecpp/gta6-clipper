package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func TestBusinessMetrics(t *testing.T) {
	p := New()
	p.MomentDetected("a")
	p.SignalZ("a", domain.SignalVoice, 3.5)
	p.ClipState(domain.StateDetected)
	p.ClipState(domain.StateDetected)
	p.StageDuration("preview", 3*time.Second)
	p.PreviewLatency(25 * time.Second)
	p.EditorUsage(1300, domain.Usage{InputTokens: 6000, OutputTokens: 900, CacheWriteTokens: 10, CacheReadTokens: 20})
	p.Decision("a", true)
	p.Decision("a", false)
	p.Decision("a", true)
	p.QueueDepth("produce", 2)
	p.SetStreamsLive(3)
	p.SetNVENC(true)
	p.ExternalError("editor")

	checks := []struct{ got, want float64 }{
		{testutil.ToFloat64(p.momentsDetected.WithLabelValues("a")), 1},
		{testutil.ToFloat64(p.signalZ.WithLabelValues("a", "voice")), 3.5},
		{testutil.ToFloat64(p.clipsTotal.WithLabelValues("detected")), 2},
		{testutil.ToFloat64(p.editorCost), 1300},
		{testutil.ToFloat64(p.editorTokens.WithLabelValues("input")), 6000},
		{testutil.ToFloat64(p.approvalRatio.WithLabelValues("a")) * 3, 2},
		{testutil.ToFloat64(p.queueDepth.WithLabelValues("produce")), 2},
		{testutil.ToFloat64(p.streamsLive), 3},
		{testutil.ToFloat64(p.nvencAvailable), 1},
		{testutil.ToFloat64(p.externalErrors.WithLabelValues("editor")), 1},
	}
	for i, c := range checks {
		if diff := c.got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("check %d: metric = %v, want %v", i, c.got, c.want)
		}
	}
	p.SetNVENC(false)
	if testutil.ToFloat64(p.nvencAvailable) != 0 {
		t.Error("nvenc gauge")
	}

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, name := range []string{"clipper_preview_latency_seconds_bucket", "clipper_stage_duration_seconds_count", "clipper_gpu_memory_bytes", "go_goroutines"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("/metrics lacks %s", name)
		}
	}
	if p.Registry() == nil {
		t.Fatal("registry")
	}
}

func TestGPUSampler(t *testing.T) {
	p := New()
	s := p.NewGPUSampler(func(context.Context) ([]byte, error) { return []byte("37, 2048\n"), nil }, nil)
	if err := s.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(p.gpuUtilization) != 0.37 || testutil.ToFloat64(p.gpuMemory) != 2048*1024*1024 {
		t.Fatalf("gpu = %v %v", testutil.ToFloat64(p.gpuUtilization), testutil.ToFloat64(p.gpuMemory))
	}
	bad := p.NewGPUSampler(func(context.Context) ([]byte, error) { return []byte("[N/A], x"), nil }, nil)
	if err := bad.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	failing := p.NewGPUSampler(func(context.Context) ([]byte, error) { return nil, errors.New("no nvidia-smi") }, nil)
	if err := failing.Sample(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestGPUSamplerReportsTheGPULeavingAndComingBack(t *testing.T) {
	ctx := context.Background()
	var gpuErr error
	s := New().NewGPUSampler(func(context.Context) ([]byte, error) {
		if gpuErr != nil {
			return nil, gpuErr
		}
		return []byte("10, 1024\n"), nil
	}, nil)
	var changes []bool
	s.OnChange(func(_ context.Context, up bool) { changes = append(changes, up) })
	sample := func() error {
		t.Helper()
		return s.Sample(ctx)
	}

	if err := sample(); err != nil || len(changes) != 0 {
		t.Fatalf("healthy sample: err=%v changes=%v", err, changes)
	}
	gpuErr = errors.New("nvidia-smi: exit status 255")
	if err := sample(); err == nil || len(changes) != 0 {
		t.Fatalf("one failure is a hiccup: err=%v changes=%v", err, changes)
	}
	if err := sample(); err == nil || !slices.Equal(changes, []bool{false}) {
		t.Fatalf("second failure: err=%v changes=%v", err, changes)
	}
	// Already reported: no more errors to log, no more alerts.
	for range 3 {
		if err := sample(); err != nil {
			t.Fatalf("down sample err = %v", err)
		}
	}
	if !slices.Equal(changes, []bool{false}) {
		t.Fatalf("changes = %v", changes)
	}
	gpuErr = nil
	if err := sample(); err != nil || !slices.Equal(changes, []bool{false, true}) {
		t.Fatalf("recovery: err=%v changes=%v", err, changes)
	}
	// A single failure after the recovery is not a new alert.
	gpuErr = errors.New("again")
	_ = sample() // the error is the expected hiccup
	gpuErr = nil
	if err := sample(); err != nil || !slices.Equal(changes, []bool{false, true}) {
		t.Fatalf("hiccup after recovery: err=%v changes=%v", err, changes)
	}
}
