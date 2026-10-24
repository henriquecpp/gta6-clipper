// Package httpserver runs the internal HTTP endpoint of the app
// (/healthz, /readyz, /metrics) and shuts it down with the process context.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server is the internal HTTP server. It is exposed only on localhost by Compose.
type Server struct {
	addr            string
	shutdownTimeout time.Duration
	log             *slog.Logger
	metrics         http.Handler
	ready           atomic.Bool
	listener        net.Listener
}

// Option configures a Server.
type Option func(*Server)

// WithShutdownTimeout bounds how long in-flight requests may take on shutdown.
func WithShutdownTimeout(d time.Duration) Option { return func(s *Server) { s.shutdownTimeout = d } }

// WithMetrics mounts the Prometheus handler on /metrics.
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// New creates a server bound to addr (e.g. ":8080").
func New(addr string, opts ...Option) *Server {
	s := &Server{addr: addr, shutdownTimeout: 10 * time.Second, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SetReady flips the /readyz answer.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Listen binds the address; Run must be called afterwards. It lets callers
// learn the bound address (Addr) before serving, which tests rely on.
func (s *Server) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	s.listener = l
	return nil
}

// Addr returns the bound address, or the configured one before Listen.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// Handler returns the router; exported for tests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n")) // a failed write means the client went away
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n")) // a failed write means the client went away
			return
		}
		_, _ = w.Write([]byte("ready\n")) // a failed write means the client went away
	})
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	return mux
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	if s.listener == nil {
		if err := s.Listen(ctx); err != nil {
			return err
		}
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(s.listener) }()
	s.log.Info("http server listening", slog.String("addr", s.Addr()))

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http serve: %w", err)
	case <-ctx.Done():
	}
	s.SetReady(false)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http serve: %w", err)
	}
	return nil
}
