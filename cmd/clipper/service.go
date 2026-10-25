package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite"
	"github.com/henriquecpp/gta6-clipper/internal/platform/httpserver"
	"github.com/henriquecpp/gta6-clipper/internal/platform/logging"
)

func runService(_ []string, stderr io.Writer) error {
	log, err := logging.New(stderr, os.Getenv("LOG_LEVEL"), logging.FormatJSON)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, envOr("CLIPPER_DB", "/data/clipper.db"))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Error("close database", logging.Err(cerr))
		}
	}()

	reg := prometheus.NewRegistry()
	srv := httpserver.New(envOr("CLIPPER_HTTP_ADDR", ":8080"),
		httpserver.WithLogger(log),
		httpserver.WithMetrics(promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	srv.SetReady(true)
	log.Info("clipper started")

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("service: %w", err)
	}
	log.Info("clipper stopped", slog.String("reason", context.Cause(ctx).Error()))
	return nil
}

func backup(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: clipper backup DEST")
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, envOr("CLIPPER_DB", "/data/clipper.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // read-only use; the backup error is what matters
	if err := sqlite.Backup(ctx, db, args[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "backup written to %s\n", args[0])
	return nil
}

func twitchAuth(_ []string, _ io.Writer) error {
	return errors.New("not implemented yet (Phase 1)")
}
