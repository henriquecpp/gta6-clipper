package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerLifecycle(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("m 1\n")) })
	s := New("127.0.0.1:0", WithLogger(slog.New(slog.DiscardHandler)), WithMetrics(metrics), WithShutdownTimeout(time.Second))
	if err := s.Listen(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	base := "http://" + s.Addr()
	if code, body := get(t, base+"/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	if code, _ := get(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d", code)
	}
	s.SetReady(true)
	if code, _ := get(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("readyz after ready = %d", code)
	}
	if code, body := get(t, base+"/metrics"); code != http.StatusOK || body != "m 1\n" {
		t.Fatalf("metrics = %d %q", code, body)
	}

	cancel() // what SIGTERM does via signal.NotifyContext
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestListenError(t *testing.T) {
	s := New("256.0.0.1:99999")
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected listen error")
	}
	if s.Addr() != "256.0.0.1:99999" {
		t.Fatal("Addr should return configured address before Listen")
	}
}
