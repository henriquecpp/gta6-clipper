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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/scheduler"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/twitchingest"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/twitchapi"
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/platform/clock"
	"github.com/henriquecpp/gta6-clipper/internal/platform/config"
	"github.com/henriquecpp/gta6-clipper/internal/platform/httpserver"
	"github.com/henriquecpp/gta6-clipper/internal/platform/logging"
)

func configPath() string { return envOr("CLIPPER_CONFIG", "/etc/clipper/config.yaml") }

func loadConfig() (*config.Config, error) {
	return config.Load(configPath(), os.Getenv)
}

func runService(_ []string, stderr io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, logging.FormatJSON)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	clk := clock.Real{}

	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Error("close database", logging.Err(cerr))
		}
	}()
	repo := sqlite.NewRepo(db)
	streamers := cfg.DomainStreamers()
	if err := repo.SyncStreamers(ctx, streamers, clk.Now()); err != nil {
		return err
	}

	detectMoment := app.NewDetectMoment(detectionConfig(cfg), repo, newClipID, clk, log)

	var tasks []scheduler.Task
	var supervisor *twitchingest.Supervisor
	if cfg.Secrets.HasTwitch() {
		tw := twitchapi.New(cfg.Secrets.TwitchClientID, cfg.Secrets.TwitchClientSecret, twitchapi.WithLogger(log))
		userTokens := tw.NewUserTokens(cfg.Twitch.TokenFile)
		if err := userTokens.Load(); err == nil {
			err = userTokens.Validate(ctx)
			if err != nil {
				log.Warn("twitch user token invalid: chat signal disabled until `task auth:twitch` is run", logging.Err(err))
				userTokens = nil
			}
		} else {
			log.Warn("twitch user token unavailable: chat signal disabled until `task auth:twitch` is run", logging.Err(err))
			userTokens = nil
		}
		var supOpts []twitchingest.SupervisorOption
		if userTokens != nil {
			supOpts = append(supOpts, twitchingest.WithChat(userTokens))
		}
		supervisor = twitchingest.NewSupervisor(twitchingest.SupervisorConfig{
			Tick:             cfg.Detection.Tick.D(),
			ClipPollInterval: cfg.Detection.Clips.PollInterval.D(),
			ClipLookback:     cfg.Capture.BufferDuration.D(),
			MaxStreams:       cfg.Concurrency.MaxStreams,
		}, detectMoment, tw, log, supOpts...)
		monitor := app.NewMonitorLives(tw, streamers, supervisor, log)
		tasks = append(tasks,
			scheduler.Task{Name: "live-poll", Interval: cfg.Twitch.LivePollInterval.D(), Jitter: 0.1, Immediate: true, Run: monitor.Poll},
			scheduler.Task{Name: "token-validate", Interval: time.Hour, Jitter: 0.05, Run: func(ctx context.Context) error {
				errs := []error{tw.ValidateApp(ctx)}
				if userTokens != nil {
					errs = append(errs, userTokens.Validate(ctx))
				}
				return errors.Join(errs...)
			}},
		)
		log.Info("live monitoring enabled", slog.Int("streamers", len(monitor.Logins())), slog.Bool("chat", userTokens != nil))
	} else {
		log.Warn("TWITCH_CLIENT_ID/TWITCH_CLIENT_SECRET missing: live monitoring disabled (degraded mode)")
	}

	reg := prometheus.NewRegistry()
	srv := httpserver.New(cfg.HTTP.Addr,
		httpserver.WithLogger(log),
		httpserver.WithShutdownTimeout(cfg.HTTP.ShutdownTimeout.D()),
		httpserver.WithMetrics(promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	g.Go(func() error { return scheduler.New(log, tasks...).Run(gctx) })
	if supervisor != nil {
		g.Go(func() error { return supervisor.Run(gctx) })
	}
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
	dbPath := envOr("CLIPPER_DB", "/data/clipper.db")
	if cfg, err := loadConfig(); err == nil {
		dbPath = cfg.DBPath
	}
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // the backup error is what matters
	if err := sqlite.Backup(ctx, db, args[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "backup written to %s\n", args[0])
	return nil
}

// twitchAuth runs the Device Code Flow and stores the user token.
func twitchAuth(_ []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Secrets.TwitchClientID == "" {
		return errors.New("TWITCH_CLIENT_ID is not set (.env)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tw := twitchapi.New(cfg.Secrets.TwitchClientID, cfg.Secrets.TwitchClientSecret)
	tok, err := tw.DeviceFlow(ctx, []string{twitchapi.ChatScope}, func(uri, code string) {
		_, _ = fmt.Fprintf(stdout, "Open %s and enter the code: %s\nWaiting for authorization...\n", uri, code)
	})
	if err != nil {
		return err
	}
	store := tw.NewUserTokens(cfg.Twitch.TokenFile)
	if err := store.Store(tok); err != nil {
		return err
	}
	if err := store.Validate(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Twitch user token stored in %s (user id %s)\n", cfg.Twitch.TokenFile, store.UserID())
	return nil
}
