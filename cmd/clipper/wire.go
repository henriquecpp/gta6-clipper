package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/claude"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/youtube"
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/domain/detect"
	"github.com/henriquecpp/gta6-clipper/internal/platform/config"
)

// newClipID generates UUID v7 clip ids (CLAUDE.md §6).
func newClipID() (domain.ClipID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate uuid v7: %w", err)
	}
	return domain.ClipID(id.String()), nil
}

// detectionConfig maps the YAML detection settings to the use case config.
func detectionConfig(c *config.Config) app.DetectionConfig {
	d := c.Detection
	weights := make(map[domain.SignalKind]float64, len(d.Weights))
	for k, w := range d.Weights {
		weights[domain.SignalKind(k)] = w
	}
	return app.DetectionConfig{
		Tick: d.Tick.D(),
		Scorer: detect.ScorerConfig{
			Weights:      weights,
			ThresholdOn:  d.ThresholdOn,
			ThresholdOff: d.ThresholdOff,
			ZMax:         d.ZMax,
			MinSignalZ:   d.MinSignalZ,
			Warmup:       d.Warmup.D(),
			Cooldown:     d.Cooldown.D(),
			MaxEpisode:   d.PostRoll.D(),
		},
		Voice: detect.VoiceConfig{
			Window:     d.Voice.Window.D(),
			Alpha:      d.Voice.EWMAAlpha,
			StaleAfter: d.Voice.StaleAfter.D(),
		},
		Chat: detect.ChatConfig{
			Window:  d.Chat.Window.D(),
			Sustain: d.Chat.Sustain.D(),
			Alpha:   d.Chat.EWMAAlpha,
		},
		TermsByLanguage: d.Chat.Emotes,
		Clips: detect.ClipsConfig{
			GroupWindow: d.Clips.GroupWindow.D(),
			MinClips:    d.Clips.MinClips,
			MinZ:        d.Clips.MinZ,
		},
		Calibration: detect.CalibrationConfig{
			Window: d.Calibration.Window, StepUp: d.Calibration.StepUp, StepDown: d.Calibration.StepDown, Max: d.Calibration.MaxOffset,
		},
		PreRoll:        d.PreRoll.D(),
		PostRoll:       d.PostRoll.D(),
		BufferDuration: c.Capture.BufferDuration.D(),
		RetainMargin:   c.Capture.SegmentDuration.D() + time.Second,
	}
}

// streamerMap indexes the allowlist by login (layouts for the renders).
func streamerMap(c *config.Config) map[string]domain.Streamer {
	streamers := map[string]domain.Streamer{}
	for _, s := range c.DomainStreamers() {
		streamers[s.Login] = s
	}
	return streamers
}

// previewConfig maps the render settings to the vertical preview.
func previewConfig(c *config.Config) app.PreviewConfig {
	return app.PreviewConfig{
		Duration:  c.Render.PreviewDuration.D(),
		Before:    c.Render.PreviewBefore.D(),
		Width:     c.Render.PreviewWidth(),
		Height:    c.Render.PreviewHeight,
		FPS:       c.Render.FPS,
		MaxBytes:  int64(c.Render.MaxFileBytes),
		Streamers: streamerMap(c),
	}
}

// produceConfig maps the render/editor settings to the production use case.
func produceConfig(c *config.Config) app.ProduceConfig {
	streamers := streamerMap(c)
	p := c.Editor.Pricing
	return app.ProduceConfig{
		MinDuration:      c.Render.MinDuration.D(),
		MaxDuration:      c.Render.MaxDuration.D(),
		PeakTolerance:    c.Editor.PeakTolerance.D(),
		HeadlineMaxChars: c.Render.HeadlineMaxChars,
		OutputLanguage:   "pt-BR",
		SnapMargin:       150 * time.Millisecond,
		Pricing: domain.Pricing{
			Input: domain.USDToMicro(p.Input), Output: domain.USDToMicro(p.Output),
			CacheWrite: domain.USDToMicro(p.CacheWrite), CacheRead: domain.USDToMicro(p.CacheRead),
		},
		MaxCost:      domain.USDToMicro(c.Editor.MaxCostPerClipUSD),
		Width:        c.Render.Width,
		Height:       c.Render.Height,
		FPS:          c.Render.FPS,
		MaxBytes:     int64(c.Render.MaxFileBytes),
		Streamers:    streamers,
		GameLanguage: c.Whisper.GameLanguage,
	}
}

// newEditor builds the Claude editor, or a disabled one (degraded mode, ADR 0016).
func newEditor(c *config.Config, log *slog.Logger) app.Editor {
	if !c.Secrets.HasAnthropic() {
		log.Warn("ANTHROPIC_API_KEY/ANTHROPIC_MODEL missing: approved clips will fail at the edit stage (degraded mode)")
		return disabledEditor{}
	}
	if c.Editor.Pricing == (config.Pricing{}) {
		log.Warn("editor.pricing is zero: costs will be recorded as $0 (fill in the current prices of ANTHROPIC_MODEL)")
	}
	return claude.New(claude.Config{
		APIKey:     c.Secrets.AnthropicAPIKey,
		Model:      c.Secrets.AnthropicModel,
		MaxTokens:  c.Editor.MaxTokens,
		Timeout:    c.Editor.Timeout.D(),
		ToolChoice: c.Editor.ToolChoice,
	})
}

type disabledEditor struct{}

func (disabledEditor) Plan(context.Context, domain.EditRequest) (domain.EditProposal, domain.Usage, error) {
	return domain.EditProposal{}, domain.Usage{}, notConfigured("anthropic is not configured (ANTHROPIC_API_KEY / ANTHROPIC_MODEL)")
}

// publishConfig maps the youtube settings to the publication use case.
func publishConfig(c *config.Config) app.PublishConfig {
	return app.PublishConfig{
		Upload: domain.UploadOptions{
			TitleHashtags: c.YouTube.TitleHashtags,
			Hashtags:      c.YouTube.Hashtags,
			Tags:          c.YouTube.Tags,
			TextLanguage:  "pt-BR", // the editor writes headline and post text in pt-BR
		},
		Streamers: streamerMap(c),
	}
}

// newYouTube builds the YouTube client (the OAuth client comes from .env).
func newYouTube(c *config.Config, log *slog.Logger) *youtube.Client {
	return youtube.New(youtube.Config{
		ClientID:          c.Secrets.YouTubeClientID,
		ClientSecret:      c.Secrets.YouTubeClientSecret,
		TokenFile:         c.YouTube.TokenFile,
		Privacy:           c.YouTube.Privacy,
		CategoryID:        c.YouTube.CategoryID,
		NotifySubscribers: c.YouTube.NotifySubscribers,
		MadeForKids:       c.YouTube.MadeForKids,
		UploadTimeout:     c.YouTube.UploadTimeout.D(),
	}, youtube.WithLogger(log))
}

// newPublisher returns the YouTube publisher and whether publishing is
// enabled: it needs the OAuth client and a stored channel authorization.
func newPublisher(c *config.Config, log *slog.Logger) (app.Publisher, bool) {
	if !c.Secrets.HasYouTube() {
		log.Info("YOUTUBE_CLIENT_ID/YOUTUBE_CLIENT_SECRET missing: publishing on YouTube disabled")
		return disabledPublisher{}, false
	}
	yt := newYouTube(c, log)
	if err := yt.Load(); err != nil {
		log.Warn("youtube channel not authorized: publishing disabled until `task auth:youtube` is run and the app restarted",
			slog.String("err", err.Error()))
		return disabledPublisher{}, false
	}
	log.Info("publishing on YouTube enabled", slog.String("privacy", c.YouTube.Privacy))
	return yt, true
}

// errYouTubeDisabled is the failure of a publication without YouTube.
var errYouTubeDisabled error = notConfigured("youtube is not configured (YOUTUBE_CLIENT_ID / YOUTUBE_CLIENT_SECRET / task auth:youtube)")

// disabledPublisher keeps the publication use case wired without YouTube.
type disabledPublisher struct{}

func (disabledPublisher) Publish(context.Context, domain.MediaRef, domain.UploadMeta) (domain.Publication, error) {
	return domain.Publication{}, errYouTubeDisabled
}
