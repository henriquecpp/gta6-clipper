// Package config loads the typed configuration from config.yaml (behaviour)
// and the environment (secrets), and validates it at startup so the process
// fails fast with a clear message (CLAUDE.md §6, §15).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Config is the whole configuration.
type Config struct {
	Streamers   []Streamer  `yaml:"streamers"`
	Twitch      Twitch      `yaml:"twitch"`
	Capture     Capture     `yaml:"capture"`
	Approval    Approval    `yaml:"approval"`
	Concurrency Concurrency `yaml:"concurrency"`
	Render      Render      `yaml:"render"`
	Whisper     Whisper     `yaml:"whisper"`
	Editor      Editor      `yaml:"editor"`
	HTTP        HTTP        `yaml:"http"`
	Detection   Detection   `yaml:"detection"`
	DBPath      string      `yaml:"db_path"`

	// Secrets come only from the environment and are never logged.
	Secrets Secrets `yaml:"-"`
	// LogLevel comes from LOG_LEVEL.
	LogLevel string `yaml:"-"`
}

// Streamer is an allowlist entry.
type Streamer struct {
	Login       string  `yaml:"login"`
	Language    string  `yaml:"language"`
	AllowsClips bool    `yaml:"allows_clips"`
	Layout      string  `yaml:"layout"`
	Stack       *Stack  `yaml:"stack"`
	Offsets     Offsets `yaml:"offsets"`
}

// Stack holds the stack layout regions as [x, y, w, h] in 0–1.
type Stack struct {
	Facecam  []float64 `yaml:"facecam"`
	Gameplay []float64 `yaml:"gameplay"`
}

// Offsets align signals with the captured timeline (§9).
type Offsets struct {
	CaptureLatency *Duration `yaml:"capture_latency"`
	Chat           *Duration `yaml:"chat"`
	Clip           *Duration `yaml:"clip"`
}

// Twitch settings.
type Twitch struct {
	LivePollInterval Duration `yaml:"live_poll_interval"`
	CaptureQuality   string   `yaml:"capture_quality"`
	TokenFile        string   `yaml:"token_file"`
}

// Capture settings.
type Capture struct {
	BufferDir       string   `yaml:"buffer_dir"`
	PendingDir      string   `yaml:"pending_dir"`
	BufferDuration  Duration `yaml:"buffer_duration"`
	BufferMaxBytes  ByteSize `yaml:"buffer_max_bytes"`
	SegmentDuration Duration `yaml:"segment_duration"`
}

// Approval settings.
type Approval struct {
	TTL Duration `yaml:"ttl"`
}

// Concurrency limits.
type Concurrency struct {
	MaxStreams          int `yaml:"max_streams"`
	FFmpegWorkers       int `yaml:"ffmpeg_workers"`
	FFmpegThreadsPerJob int `yaml:"ffmpeg_threads_per_job"`
	TranscribeWorkers   int `yaml:"transcribe_workers"`
	RenderWorkers       int `yaml:"render_workers"`
}

// Render settings.
type Render struct {
	OutputDir        string   `yaml:"output_dir"`
	Width            int      `yaml:"width"`
	Height           int      `yaml:"height"`
	FPS              int      `yaml:"fps"`
	Encoder          string   `yaml:"encoder"`
	MinDuration      Duration `yaml:"min_duration"`
	MaxDuration      Duration `yaml:"max_duration"`
	MaxFileBytes     ByteSize `yaml:"max_file_bytes"`
	PreviewHeight    int      `yaml:"preview_height"`
	PreviewDuration  Duration `yaml:"preview_duration"`
	Font             string   `yaml:"font"`
	MaxLineChars     int      `yaml:"max_line_chars"`
	HeadlineMaxChars int      `yaml:"headline_max_chars"`
}

// Whisper settings.
type Whisper struct {
	URL     string   `yaml:"url"`
	Timeout Duration `yaml:"timeout"`
}

// Editor (Claude) settings.
type Editor struct {
	MaxTokens         int64    `yaml:"max_tokens"`
	Timeout           Duration `yaml:"timeout"`
	MaxCostPerClipUSD float64  `yaml:"max_cost_per_clip_usd"`
	ToolChoice        string   `yaml:"tool_choice"`
	PeakTolerance     Duration `yaml:"peak_tolerance"`
	Pricing           Pricing  `yaml:"pricing"`
}

// Pricing in USD per million tokens.
type Pricing struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheWrite float64 `yaml:"cache_write"`
	CacheRead  float64 `yaml:"cache_read"`
}

// HTTP server settings.
type HTTP struct {
	Addr            string   `yaml:"addr"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

// Detection settings (§9).
type Detection struct {
	Tick         Duration           `yaml:"tick"`
	Warmup       Duration           `yaml:"warmup"`
	Cooldown     Duration           `yaml:"cooldown"`
	ThresholdOn  float64            `yaml:"threshold_on"`
	ThresholdOff float64            `yaml:"threshold_off"`
	MinSignalZ   float64            `yaml:"min_signal_z"`
	ZMax         float64            `yaml:"z_max"`
	PreRoll      Duration           `yaml:"pre_roll"`
	PostRoll     Duration           `yaml:"post_roll"`
	Weights      map[string]float64 `yaml:"weights"`
	Voice        VoiceDetection     `yaml:"voice"`
	Chat         ChatDetection      `yaml:"chat"`
	Clips        ClipsDetection     `yaml:"clips"`
}

// VoiceDetection settings.
type VoiceDetection struct {
	Frame      Duration `yaml:"frame"`
	Window     Duration `yaml:"window"`
	EWMAAlpha  float64  `yaml:"ewma_alpha"`
	StaleAfter Duration `yaml:"stale_after"`
}

// ChatDetection settings.
type ChatDetection struct {
	Window    Duration                      `yaml:"window"`
	Sustain   Duration                      `yaml:"sustain"`
	EWMAAlpha float64                       `yaml:"ewma_alpha"`
	Emotes    map[string]map[string]float64 `yaml:"emotes"`
}

// ClipsDetection settings.
type ClipsDetection struct {
	PollInterval Duration `yaml:"poll_interval"`
	GroupWindow  Duration `yaml:"group_window"`
	MinClips     int      `yaml:"min_clips"`
	// MinZ: how far above the channel's usual clip rate a cluster must be.
	MinZ float64 `yaml:"min_z"`
}

// Secrets read from the environment (.env).
type Secrets struct {
	TwitchClientID         string
	TwitchClientSecret     string
	TelegramBotToken       string
	TelegramAllowedChatIDs []int64
	AnthropicAPIKey        string
	AnthropicModel         string
}

// HasTwitch reports whether Twitch credentials are present.
func (s Secrets) HasTwitch() bool { return s.TwitchClientID != "" && s.TwitchClientSecret != "" }

// HasTelegram reports whether the Telegram bot is configured.
func (s Secrets) HasTelegram() bool {
	return s.TelegramBotToken != "" && len(s.TelegramAllowedChatIDs) > 0
}

// HasAnthropic reports whether the editor can call Claude.
func (s Secrets) HasAnthropic() bool { return s.AnthropicAPIKey != "" && s.AnthropicModel != "" }

// Load reads the YAML file at path, applies defaults, reads secrets with
// getenv and validates everything.
func Load(path string, getenv func(string) string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(raw, getenv)
}

// Parse is Load without the file read.
func Parse(raw []byte, getenv func(string) string) (*Config, error) {
	cfg := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)                                              // typos in keys are errors, not silent defaults
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) { // empty file = all defaults
		return nil, fmt.Errorf("parse config: %w", err)
	}
	secrets, err := readSecrets(getenv)
	if err != nil {
		return nil, err
	}
	cfg.Secrets = secrets
	cfg.LogLevel = getenv("LOG_LEVEL")
	if v := getenv("WHISPER_URL"); v != "" {
		cfg.Whisper.URL = v
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func readSecrets(getenv func(string) string) (Secrets, error) {
	s := Secrets{
		TwitchClientID:     strings.TrimSpace(getenv("TWITCH_CLIENT_ID")),
		TwitchClientSecret: strings.TrimSpace(getenv("TWITCH_CLIENT_SECRET")),
		TelegramBotToken:   strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN")),
		AnthropicAPIKey:    strings.TrimSpace(getenv("ANTHROPIC_API_KEY")),
		AnthropicModel:     strings.TrimSpace(getenv("ANTHROPIC_MODEL")),
	}
	for _, part := range strings.Split(getenv("TELEGRAM_ALLOWED_CHAT_IDS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return Secrets{}, fmt.Errorf("TELEGRAM_ALLOWED_CHAT_IDS: %q is not an integer chat id", part)
		}
		s.TelegramAllowedChatIDs = append(s.TelegramAllowedChatIDs, id)
	}
	return s, nil
}

// DomainStreamers converts the allowlist to domain values (Validate already ran).
func (c *Config) DomainStreamers() []domain.Streamer {
	out := make([]domain.Streamer, 0, len(c.Streamers))
	for _, s := range c.Streamers {
		out = append(out, s.domain())
	}
	return out
}

// defaultOffsets are used until measured per streamer (§9, Phases 2/3).
var defaultOffsets = domain.Offsets{CaptureLatency: 4 * time.Second, Chat: 8 * time.Second, Clip: 5 * time.Second}

func (s Streamer) domain() domain.Streamer {
	d := domain.Streamer{
		Login:       strings.ToLower(strings.TrimSpace(s.Login)),
		Language:    strings.ToLower(strings.TrimSpace(s.Language)),
		AllowsClips: s.AllowsClips,
		Layout:      domain.Layout(s.Layout),
		Offsets:     defaultOffsets,
	}
	if d.Layout == "" {
		d.Layout = domain.LayoutBlur
	}
	if s.Offsets.CaptureLatency != nil {
		d.Offsets.CaptureLatency = s.Offsets.CaptureLatency.D()
	}
	if s.Offsets.Chat != nil {
		d.Offsets.Chat = s.Offsets.Chat.D()
	}
	if s.Offsets.Clip != nil {
		d.Offsets.Clip = s.Offsets.Clip.D()
	}
	if s.Stack != nil {
		d.Stack = domain.StackRegions{Facecam: rect(s.Stack.Facecam), Gameplay: rect(s.Stack.Gameplay)}
	}
	return d
}

func rect(v []float64) domain.Rect {
	if len(v) != 4 {
		return domain.Rect{}
	}
	return domain.Rect{X: v[0], Y: v[1], W: v[2], H: v[3]}
}
