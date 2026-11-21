package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Validate checks every value and returns all problems at once.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	pos := func(name string, d Duration) {
		if d <= 0 {
			add("%s must be > 0 (got %s)", name, d.D())
		}
	}

	seen := map[string]bool{}
	for i, s := range c.Streamers {
		ds := s.domain()
		if seen[ds.Login] {
			add("streamers[%d]: duplicate login %q", i, ds.Login)
		}
		seen[ds.Login] = true
		if s.Stack != nil && (len(s.Stack.Facecam) != 4 || len(s.Stack.Gameplay) != 4) {
			add("streamers[%d] (%s): stack regions must be [x, y, w, h]", i, ds.Login)
		}
		if err := ds.Validate(); err != nil {
			add("streamers[%d]: %v", i, err)
		}
		if ds.Language == "" {
			add("streamers[%d] (%s): language is required (ISO 639-1, e.g. en, pt)", i, ds.Login)
		}
	}

	pos("twitch.live_poll_interval", c.Twitch.LivePollInterval)
	if c.Twitch.LivePollInterval.D() > 0 && c.Twitch.LivePollInterval.D() < 15*time.Second {
		add("twitch.live_poll_interval must be >= 15s to respect rate limits")
	}
	if strings.TrimSpace(c.Twitch.CaptureQuality) == "" {
		add("twitch.capture_quality is required")
	}
	if c.Twitch.TokenFile == "" {
		add("twitch.token_file is required")
	}
	for i, g := range c.Twitch.Games {
		if strings.TrimSpace(g) == "" {
			add("twitch.games[%d] is blank (use a category name or id; an empty list monitors every game)", i)
		}
	}

	if c.Capture.BufferDir == "" || c.Capture.PendingDir == "" {
		add("capture.buffer_dir and capture.pending_dir are required")
	}
	pos("capture.buffer_duration", c.Capture.BufferDuration)
	pos("capture.segment_duration", c.Capture.SegmentDuration)
	if c.Capture.BufferMaxBytes <= 0 {
		add("capture.buffer_max_bytes must be > 0")
	}
	if c.Capture.BufferDuration.D() < c.Detection.PreRoll.D()+c.Detection.PostRoll.D()+c.Capture.SegmentDuration.D() {
		add("capture.buffer_duration (%s) must exceed detection.pre_roll + post_roll + one segment", c.Capture.BufferDuration.D())
	}

	pos("approval.ttl", c.Approval.TTL)

	cc := c.Concurrency
	if cc.MaxStreams < 1 || cc.FFmpegWorkers < 1 || cc.FFmpegThreadsPerJob < 1 || cc.TranscribeWorkers < 1 || cc.RenderWorkers < 1 {
		add("concurrency values must all be >= 1")
	}

	r := c.Render
	if r.OutputDir == "" {
		add("render.output_dir is required")
	}
	if r.Width <= 0 || r.Height <= 0 || r.Width%2 != 0 || r.Height%2 != 0 {
		add("render.width/height must be positive even numbers")
	}
	if r.FPS != 30 && r.FPS != 60 {
		add("render.fps must be 30 or 60")
	}
	if r.Encoder != "h264_nvenc" && r.Encoder != "libx264" {
		add("render.encoder must be h264_nvenc or libx264")
	}
	pos("render.min_duration", r.MinDuration)
	if r.MaxDuration <= r.MinDuration {
		add("render.max_duration must be greater than render.min_duration")
	}
	if r.MaxFileBytes <= 0 || r.MaxFileBytes > 50<<20 {
		add("render.max_file_bytes must be in (0, 50MiB] (Telegram upload limit)")
	}
	if r.PreviewHeight < 144 || r.PreviewHeight%2 != 0 {
		add("render.preview_height must be an even number >= 144")
	}
	pos("render.preview_duration", r.PreviewDuration)
	pos("render.preview_before", r.PreviewBefore)
	if r.PreviewBefore >= r.PreviewDuration {
		add("render.preview_before (%s) must be shorter than render.preview_duration (%s), or the preview ends at the peak",
			r.PreviewBefore.D(), r.PreviewDuration.D())
	}
	if r.MaxLineChars < 10 {
		add("render.max_line_chars must be >= 10")
	}
	if r.HeadlineMaxChars < 10 {
		add("render.headline_max_chars must be >= 10")
	}
	if r.Font == "" {
		add("render.font is required")
	}

	if u, err := url.Parse(c.Whisper.URL); err != nil || u.Scheme == "" || u.Host == "" {
		add("whisper.url must be an absolute URL (got %q)", c.Whisper.URL)
	}
	pos("whisper.timeout", c.Whisper.Timeout)

	e := c.Editor
	if e.MaxTokens < 256 || e.MaxTokens > 64000 {
		add("editor.max_tokens must be in [256, 64000]")
	}
	pos("editor.timeout", e.Timeout)
	if e.MaxCostPerClipUSD <= 0 {
		add("editor.max_cost_per_clip_usd must be > 0")
	}
	if e.ToolChoice != "tool" && e.ToolChoice != "auto" {
		add("editor.tool_choice must be tool or auto")
	}
	if e.PeakTolerance < 0 {
		add("editor.peak_tolerance must be >= 0")
	}
	if e.Pricing.Input < 0 || e.Pricing.Output < 0 || e.Pricing.CacheWrite < 0 || e.Pricing.CacheRead < 0 {
		add("editor.pricing values must be >= 0")
	}

	if c.HTTP.Addr == "" {
		add("http.addr is required")
	}
	pos("http.shutdown_timeout", c.HTTP.ShutdownTimeout)
	if c.DBPath == "" {
		add("db_path is required")
	}

	errs = append(errs, c.Detection.validate()...)

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	return nil
}

func (d Detection) validate() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for name, v := range map[string]Duration{
		"detection.tick": d.Tick, "detection.cooldown": d.Cooldown, "detection.pre_roll": d.PreRoll,
		"detection.post_roll": d.PostRoll, "detection.voice.frame": d.Voice.Frame,
		"detection.voice.window": d.Voice.Window, "detection.voice.stale_after": d.Voice.StaleAfter,
		"detection.chat.window": d.Chat.Window, "detection.chat.sustain": d.Chat.Sustain,
		"detection.clips.poll_interval": d.Clips.PollInterval, "detection.clips.group_window": d.Clips.GroupWindow,
	} {
		if v <= 0 {
			add("%s must be > 0", name)
		}
	}
	if d.Warmup < 0 {
		add("detection.warmup must be >= 0")
	}
	if d.ThresholdOn <= 0 || d.ThresholdOff < 0 || d.ThresholdOff >= d.ThresholdOn {
		add("detection thresholds must satisfy 0 <= threshold_off < threshold_on")
	}
	if d.ZMax <= 0 || d.MinSignalZ < 0 || d.MinSignalZ > d.ZMax {
		add("detection.z_max must be > 0 and 0 <= min_signal_z <= z_max")
	}
	known := map[string]bool{string(domain.SignalVoice): true, string(domain.SignalChat): true, string(domain.SignalClips): true}
	sum := 0.0
	for k, w := range d.Weights {
		if !known[k] {
			add("detection.weights: unknown signal %q (want voice, chat, clips)", k)
		}
		if w < 0 {
			add("detection.weights.%s must be >= 0", k)
		}
		sum += w
	}
	if sum <= 0 {
		add("detection.weights must have a positive sum")
	}
	if d.Voice.Window < d.Voice.Frame {
		add("detection.voice.window must be >= detection.voice.frame")
	}
	if d.Voice.Frame.D() < 10*time.Millisecond || d.Voice.Frame.D() > 100*time.Millisecond {
		add("detection.voice.frame must be in [10ms, 100ms]")
	}
	for name, a := range map[string]float64{"voice": d.Voice.EWMAAlpha, "chat": d.Chat.EWMAAlpha} {
		if a <= 0 || a >= 1 {
			add("detection.%s.ewma_alpha must be in (0, 1)", name)
		}
	}
	if d.Chat.Sustain > d.Chat.Window {
		add("detection.chat.sustain must be <= detection.chat.window")
	}
	for lang, emotes := range d.Chat.Emotes {
		for term, w := range emotes {
			if w <= 0 {
				add("detection.chat.emotes.%s.%s must be > 0", lang, term)
			}
		}
	}
	if d.Clips.MinClips < 1 {
		add("detection.clips.min_clips must be >= 1")
	}
	if d.Clips.MinZ < 0 {
		add("detection.clips.min_z must be >= 0")
	}
	if c := d.Calibration; c.Window < 1 || c.StepUp < 0 || c.StepDown < 0 || c.MaxOffset < 0 {
		add("detection.calibration needs window >= 1 and step_up, step_down, max_offset >= 0 (max_offset 0 disables it)")
	}
	if d.MinSignalZ+d.Calibration.MaxOffset > d.ZMax {
		add("detection.min_signal_z + calibration.max_offset must be <= z_max (or the gate could never open)")
	}
	return errs
}
