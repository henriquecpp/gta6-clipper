package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/scheduler"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/telegrambot"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/twitchingest"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/capture"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/render"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/resilience"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/twitchapi"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/whisper"
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/clock"
	"github.com/henriquecpp/gta6-clipper/internal/platform/config"
	"github.com/henriquecpp/gta6-clipper/internal/platform/httpserver"
	"github.com/henriquecpp/gta6-clipper/internal/platform/logging"
	"github.com/henriquecpp/gta6-clipper/internal/platform/metrics"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
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

	capt, err := capture.New(capture.Config{
		BufferDir:       cfg.Capture.BufferDir,
		PendingDir:      cfg.Capture.PendingDir,
		BufferDuration:  cfg.Capture.BufferDuration.D(),
		MaxBytes:        int64(cfg.Capture.BufferMaxBytes),
		SegmentDuration: cfg.Capture.SegmentDuration.D(),
		Quality:         cfg.Twitch.CaptureQuality,
		Frame:           cfg.Detection.Voice.Frame.D(),
		Threads:         cfg.Concurrency.FFmpegThreadsPerJob,
		Workers:         cfg.Concurrency.FFmpegWorkers,
	}, capture.WithLogger(log))
	if err != nil {
		return err
	}
	if err := pruneOrphans(ctx, repo, capt, log); err != nil {
		log.Warn("prune orphan media", logging.Err(err))
	}

	prom := metrics.New()
	errLog := &app.ErrorLog{}
	rend := render.New(ctx, render.Config{
		Encoder:        cfg.Render.Encoder,
		Threads:        cfg.Concurrency.FFmpegThreadsPerJob,
		PreviewWorkers: cfg.Concurrency.FFmpegWorkers,
		FinalWorkers:   cfg.Concurrency.RenderWorkers,
		OutputDir:      cfg.Render.OutputDir,
		Style:          render.ASSStyle{Font: cfg.Render.Font, MaxLineChars: cfg.Render.MaxLineChars},
	}, render.WithLogger(log))
	prom.SetNVENC(rend.NVENC())

	youtubePublisher, publishing := newPublisher(cfg, log)
	tg, err := newTelegram(cfg, log, publishing)
	if err != nil {
		return err
	}
	// Retry/backoff, error metrics and logs around every external port (Decorator).
	rdeps := resilience.Deps{Log: log, Metrics: prom}
	media := resilience.NewMediaCapture(capt, resilience.FFmpegPolicy, rdeps)
	renderer := resilience.NewRenderer(rend, resilience.FFmpegPolicy, rdeps)
	notifier := resilience.NewNotifier(tg.notifier, resilience.NetworkPolicy, rdeps)
	ttl := cfg.Approval.TTL.D()
	sendPreview := app.NewSendPreview(previewConfig(cfg), repo, renderer, notifier, clk, log, prom, errLog)
	detectMoment := app.NewDetectMoment(detectionConfig(cfg), repo, newClipID, clk, log,
		app.WithCapture(media), app.WithDetectMetrics(prom),
		app.WithClipHandler(func(ctx context.Context, c *domain.Clip) {
			if err := sendPreview.Execute(ctx, c.ID); err != nil {
				log.Warn("preview failed", slog.String("clip_id", string(c.ID)), logging.Err(err))
			}
		}))
	transcriber := resilience.NewTranscriber(whisper.NewTranscriber(whisper.NewClient(cfg.Whisper.URL, &http.Client{Timeout: cfg.Whisper.Timeout.D()}),
		whisper.WithWorkers(cfg.Concurrency.TranscribeWorkers), whisper.WithTimeout(cfg.Whisper.Timeout.D())), resilience.WhisperPolicy, rdeps)
	editor := resilience.NewEditor(newEditor(cfg, log), resilience.EditorPolicy, rdeps)
	produce := app.NewProduceFinalClip(produceConfig(cfg), repo, transcriber, editor, renderer, notifier, media, clk, log, prom, errLog)
	queue := app.NewJobQueue("produce", produce.Execute, max(cfg.Concurrency.TranscribeWorkers, cfg.Concurrency.RenderWorkers), 100,
		cfg.HTTP.ShutdownTimeout.D(), log, prom)
	// The approver's decisions calibrate the detection bar of each streamer.
	if err := detectMoment.Calibrate(ctx); err != nil {
		log.Warn("calibrate detection", logging.Err(err))
	}
	decisions := app.NewDecisions(repo, media, notifier, clk, log, ttl, app.WithDecisionMetrics(prom),
		app.WithProducer(func(ctx context.Context, id domain.ClipID) { queue.Submit(ctx, id) }),
		app.WithDecisionObserver(detectMoment.ObserveDecision))
	retry := app.NewRetryClip(repo, notifier, clk, log, prom, queue.Submit)
	// Publication on YouTube: only when the human taps the button; uploads
	// run one at a time and are never retried blindly (PublishPolicy).
	publisher := resilience.NewPublisher(youtubePublisher, resilience.PublishPolicy, rdeps)
	publish := app.NewPublishClip(publishConfig(cfg), repo, publisher, notifier, clk, log, prom, errLog)
	publishQueue := app.NewJobQueue("publish", publish.Execute, 1, 20, cfg.HTTP.ShutdownTimeout.D(), log, prom)
	requestPublish := app.NewRequestPublish(repo, notifier, clk, log, prom, publishQueue.Submit)
	recoverPending := app.NewRecoverPending(repo, sendPreview.Execute, queue.Submit, log,
		app.WithInterruptedPublications(publish.Interrupted))

	gpu := prom.NewGPUSampler(func(ctx context.Context) ([]byte, error) {
		return proc.Run(ctx, nil, "nvidia-smi", "--query-gpu=utilization.gpu,memory.used", "--format=csv,noheader,nounits")
	}, log)
	tasks := []scheduler.Task{
		{Name: "expire-previews", Interval: time.Minute, Immediate: true, Run: decisions.ExpireTask},
		{Name: "gpu-metrics", Interval: 15 * time.Second, Immediate: true, Run: gpu.Sample},
	}
	var lives, offGame func() []domain.LiveStream
	var supervisor *twitchingest.Supervisor
	if cfg.Secrets.HasTwitch() {
		tw := twitchapi.New(cfg.Secrets.TwitchClientID, cfg.Secrets.TwitchClientSecret, twitchapi.WithLogger(log))
		platform := resilience.NewPlatform(tw, resilience.NetworkPolicy, rdeps)
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
		supOpts := []twitchingest.SupervisorOption{twitchingest.WithCapturer(capt)}
		if userTokens != nil {
			supOpts = append(supOpts, twitchingest.WithChat(userTokens))
		}
		supervisor = twitchingest.NewSupervisor(twitchingest.SupervisorConfig{
			Tick:             cfg.Detection.Tick.D(),
			ClipPollInterval: cfg.Detection.Clips.PollInterval.D(),
			ClipLookback:     cfg.Capture.BufferDuration.D(),
			MaxStreams:       cfg.Concurrency.MaxStreams,
		}, detectMoment, platform, log, supOpts...)
		monitor := app.NewMonitorLives(platform, streamers, domain.GameFilter(cfg.Twitch.Games), supervisor, log)
		lives = monitor.Live
		offGame = monitor.OffGame
		tasks = append(tasks,
			scheduler.Task{Name: "streams-gauge", Interval: 15 * time.Second, Run: func(context.Context) error {
				prom.SetStreamsLive(supervisor.Running())
				return nil
			}},
			scheduler.Task{Name: "live-poll", Interval: cfg.Twitch.LivePollInterval.D(), Jitter: 0.1, Immediate: true, Run: monitor.Poll},
			scheduler.Task{Name: "token-validate", Interval: time.Hour, Jitter: 0.05, Run: func(ctx context.Context) error {
				errs := []error{tw.ValidateApp(ctx)}
				if userTokens != nil {
					errs = append(errs, userTokens.Validate(ctx))
				}
				return errors.Join(errs...)
			}},
		)
		log.Info("live monitoring enabled", slog.Int("streamers", len(monitor.Logins())),
			slog.Any("games", cfg.Twitch.Games), slog.Bool("chat", userTokens != nil))
	} else {
		log.Warn("TWITCH_CLIENT_ID/TWITCH_CLIENT_SECRET missing: live monitoring disabled (degraded mode)")
	}

	status := app.NewStatus(repo, clk, ttl, lives, queue.Depth, errLog)
	status.SetStrictness(detectMoment.Strictness)
	status.SetOffGame(offGame)

	srv := httpserver.New(cfg.HTTP.Addr,
		httpserver.WithLogger(log),
		httpserver.WithShutdownTimeout(cfg.HTTP.ShutdownTimeout.D()),
		httpserver.WithMetrics(prom.Handler()))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	g.Go(func() error { return queue.Run(gctx) })
	g.Go(func() error { return publishQueue.Run(gctx) })
	g.Go(func() error {
		if _, err := recoverPending.Execute(gctx); err != nil {
			log.Warn("recover pending clips", logging.Err(err))
		}
		return nil
	})
	g.Go(func() error { return scheduler.New(log, tasks...).Run(gctx) })
	if supervisor != nil {
		g.Go(func() error { return supervisor.Run(gctx) })
	}
	if tg.bot != nil {
		botOpts := []telegrambot.Option{telegrambot.WithRetry(retry.Execute)}
		if publishing {
			botOpts = append(botOpts, telegrambot.WithPublish(requestPublish.Execute))
		}
		bot := telegrambot.New(tg.bot, cfg.Secrets.TelegramAllowedChatIDs, decisions, status, log, botOpts...)
		g.Go(func() error { return bot.Run(gctx) })
	}
	srv.SetReady(true)
	log.Info("clipper started")

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("service: %w", err)
	}
	log.Info("clipper stopped", slog.String("reason", context.Cause(ctx).Error()))
	return nil
}

// pruneOrphans deletes pending media not referenced by any clip that can
// still use it (left behind by crashes).
func pruneOrphans(ctx context.Context, repo app.ClipRepository, capt *capture.Capture, log *slog.Logger) error {
	clips, err := repo.ListByState(ctx, domain.StateDetected, domain.StatePreviewSent, domain.StateApproved,
		domain.StateEditing, domain.StateRendered, domain.StateFailed)
	if err != nil {
		return err
	}
	keep := map[domain.MediaRef]bool{}
	for _, c := range clips {
		keep[c.SourceMedia], keep[c.PreviewMedia] = true, true
	}
	n, err := capt.Prune(keep)
	if n > 0 {
		log.Info("orphan media removed", slog.Int("files", n))
	}
	return err
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

// youtubeAuth authorizes the YouTube channel once (OAuth for limited-input
// devices) and shows which channel the videos will be published to.
func youtubeAuth(_ []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !cfg.Secrets.HasYouTube() {
		return errors.New("YOUTUBE_CLIENT_ID / YOUTUBE_CLIENT_SECRET are not set (.env)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	yt := newYouTube(cfg, slog.New(slog.DiscardHandler))
	err = yt.DeviceFlow(ctx, func(verificationURL, code string) {
		_, _ = fmt.Fprintf(stdout, "Abra %s e digite o código: %s\nEscolha a conta/canal onde os Shorts serão publicados. Aguardando...\n",
			verificationURL, code)
	})
	if err != nil {
		return err
	}
	title, id, err := yt.Channel(ctx)
	if err != nil {
		return fmt.Errorf("authorized, but the channel could not be read: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "Canal autorizado: %s (%s)\nToken salvo em %s. Reinicie o app (task up) para mostrar o botão de publicar.\n",
		title, id, cfg.YouTube.TokenFile)
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
