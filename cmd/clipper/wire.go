package main

import (
	"fmt"

	"github.com/google/uuid"

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
		},
		PreRoll:        d.PreRoll.D(),
		PostRoll:       d.PostRoll.D(),
		BufferDuration: c.Capture.BufferDuration.D(),
	}
}
