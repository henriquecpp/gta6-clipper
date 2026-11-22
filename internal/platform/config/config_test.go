package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "..", "config.example.yaml"), env(map[string]string{
		"TWITCH_CLIENT_ID": "id", "TWITCH_CLIENT_SECRET": "secret",
		"TELEGRAM_BOT_TOKEN": "tok", "TELEGRAM_ALLOWED_CHAT_IDS": "123, -100456",
		"ANTHROPIC_API_KEY": "k", "ANTHROPIC_MODEL": "m", "LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Secrets.HasTwitch() || !cfg.Secrets.HasTelegram() || !cfg.Secrets.HasAnthropic() {
		t.Fatalf("secrets not loaded: %+v", cfg.Secrets.TelegramAllowedChatIDs)
	}
	if got := cfg.Secrets.TelegramAllowedChatIDs; len(got) != 2 || got[1] != -100456 {
		t.Fatalf("chat ids = %v", got)
	}
	if cfg.LogLevel != "debug" || cfg.Capture.BufferMaxBytes != 4<<30 || cfg.Render.MaxFileBytes != 45<<20 {
		t.Fatalf("unexpected values: %+v", cfg)
	}
	if d := cfg.Detection; d.ThresholdOn != 4.0 || d.Clips.MinZ != 4.0 || d.Calibration != (Calibration{Window: 20, StepUp: 0.25, StepDown: 0.25, MaxOffset: 1.5}) {
		t.Fatalf("detection = %+v", d)
	}
	ss := cfg.DomainStreamers()
	if len(ss) != 1 || ss[0].Login != "exemplo_streamer" || ss[0].Offsets.Chat != 8*time.Second || ss[0].Layout != domain.LayoutAuto {
		t.Fatalf("streamers = %+v", ss)
	}
}

func TestEmptyConfigUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Detection.ThresholdOn != 4.0 || cfg.Detection.Clips.MinZ != 4.0 || cfg.Detection.Calibration.MaxOffset != 1.5 ||
		cfg.Approval.TTL.D() != 6*time.Hour || cfg.Secrets.HasTwitch() {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestWhisperURLFromEnv(t *testing.T) {
	cfg, err := Parse([]byte("whisper: {url: http://a:1}"), env(map[string]string{"WHISPER_URL": "http://b:2"}))
	if err != nil || cfg.Whisper.URL != "http://b:2" {
		t.Fatalf("url = %v, %v", cfg.Whisper.URL, err)
	}
	if cfg.Whisper.BeamSize != 5 || cfg.Whisper.GameLanguage != "en" {
		t.Fatalf("whisper defaults = %+v", cfg.Whisper)
	}
	if _, err := Parse([]byte("whisper: {beam_size: 99}"), env(nil)); err == nil {
		t.Fatal("beam_size 99 accepted")
	}
	if cfg, err := Parse([]byte("whisper: {beam_size: 0, game_language: ''}"), env(nil)); err != nil || cfg.Whisper.GameLanguage != "" {
		t.Fatalf("greedy without second pass: %+v, %v", cfg.Whisper, err)
	}
}

func TestStreamerConversion(t *testing.T) {
	yml := `
streamers:
  - login: Stacker
    language: PT
    allows_clips: true
    layout: stack
    stack: { facecam: [0.7, 0.0, 0.3, 0.3], gameplay: [0, 0, 1, 1] }
    offsets: { capture_latency: 6s, chat: 10s, clip: 30s }
`
	cfg, err := Parse([]byte(yml), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.DomainStreamers()[0]
	want := domain.Streamer{
		Login: "stacker", Language: "pt", AllowsClips: true, Layout: domain.LayoutStack,
		Stack:   domain.StackRegions{Facecam: domain.Rect{X: 0.7, W: 0.3, H: 0.3}, Gameplay: domain.Rect{W: 1, H: 1}},
		Offsets: domain.Offsets{CaptureLatency: 6 * time.Second, Chat: 10 * time.Second, Clip: 30 * time.Second},
	}
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "bogus: 1",
		"bad duration":     "approval: {ttl: soon}",
		"bad size":         "capture: {buffer_max_bytes: lots}",
		"thresholds":       "detection: {threshold_on: 1, threshold_off: 2}",
		"weights":          "detection: {weights: {voice: 1, smell: 1}}",
		"fps":              "render: {fps: 24}",
		"encoder":          "render: {encoder: vp9}",
		"tool choice":      "editor: {tool_choice: any}",
		"file too big":     "render: {max_file_bytes: 60MiB}",
		"buffer too short": "capture: {buffer_duration: 30s}",
		"poll too fast":    "twitch: {live_poll_interval: 5s}",
		"preview before":   "render: {preview_duration: 30s, preview_before: 30s}",
		"blank game":       "twitch: {games: [' ']}",
		"duplicate login":  "streamers: [{login: a, language: en, allows_clips: true}, {login: A, language: en}]",
		"missing language": "streamers: [{login: a, allows_clips: true}]",
		"bad layout":       "streamers: [{login: a, language: en, layout: diagonal}]",
		"bad stack":        "streamers: [{login: a, language: en, layout: stack, stack: {facecam: [1], gameplay: [0,0,1,1]}}]",
		"whisper url":      "whisper: {url: 'not a url'}",
		"ewma":             "detection: {chat: {ewma_alpha: 1.5}}",
		"sustain":          "detection: {chat: {window: 2s, sustain: 4s}}",
		"emote weight":     "detection: {chat: {emotes: {en: {KEKW: 0}}}}",
		"duration as int":  "approval: {ttl: 5}",
		"concurrency":      "concurrency: {render_workers: 0}",
		"min clips":        "detection: {clips: {min_clips: 0}}",
		"clips min z":      "detection: {clips: {min_z: -1}}",
		"calibration":      "detection: {calibration: {window: 0}}",
		"gate unreachable": "detection: {min_signal_z: 5, calibration: {max_offset: 2}}",
		"voice frame":      "detection: {voice: {frame: 1s, window: 2s}}",
		"pricing":          "editor: {pricing: {input: -1}}",
		"max tokens":       "editor: {max_tokens: 10}",
		"yt privacy":       "youtube: {privacy: friends}",
		"yt category":      "youtube: {category_id: gaming}",
		"yt hashtag":       "youtube: {hashtags: [GTA6]}",
		"yt title tag":     "youtube: {title_hashtags: ['#GTA 6']}",
		"yt tag comma":     "youtube: {tags: ['GTA, 6']}",
		"yt timeout":       "youtube: {upload_timeout: 0s}",
		"yt token file":    "youtube: {token_file: ''}",
	}
	for name, yml := range cases {
		if _, err := Parse([]byte(yml), env(nil)); err == nil {
			t.Errorf("%s: expected error for %q", name, yml)
		}
	}
	if _, err := Parse(nil, env(map[string]string{"TELEGRAM_ALLOWED_CHAT_IDS": "12,abc"})); err == nil || !strings.Contains(err.Error(), "abc") {
		t.Errorf("chat id error = %v", err)
	}
}

func TestByteSize(t *testing.T) {
	cases := map[string]ByteSize{"4GiB": 4 << 30, "45MiB": 45 << 20, "1.5KiB": 1536, "100": 100, "2MB": 2_000_000, "7 B": 7}
	for in, want := range cases {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "xGiB", "-2MiB"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) should fail", bad)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), os.Getenv); err == nil {
		t.Fatal("expected error")
	}
}

func TestTwitchGames(t *testing.T) {
	cases := map[string][]string{
		"": {"Grand Theft Auto VI"}, // default: only GTA VI
		"twitch: {games: [1867279146, Just Chatting]}": {"1867279146", "Just Chatting"},
		"twitch: {games: []}":                          {}, // explicit empty list: every game
	}
	for yml, want := range cases {
		cfg, err := Parse([]byte(yml), env(nil))
		if err != nil {
			t.Fatalf("%q: %v", yml, err)
		}
		if !slices.Equal(cfg.Twitch.Games, want) {
			t.Errorf("%q: games = %q, want %q", yml, cfg.Twitch.Games, want)
		}
	}
}

func TestPreviewWidthFollowsTheFinalAspect(t *testing.T) {
	cfg, err := Parse(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := cfg.Render.PreviewWidth(), cfg.Render.PreviewHeight; w != 480 || h != 854 {
		t.Fatalf("default preview = %dx%d, want 480x854", w, h)
	}
	if w := (Render{Width: 1080, Height: 1920, PreviewHeight: 1280}).PreviewWidth(); w != 720 {
		t.Fatalf("1280 high preview width = %d, want 720", w)
	}
	if w := (Render{PreviewHeight: 854}).PreviewWidth(); w != 0 {
		t.Fatalf("no final size: width = %d, want 0", w)
	}
}

func TestYouTubeDefaultsAndSecrets(t *testing.T) {
	cfg, err := Parse(nil, env(map[string]string{"YOUTUBE_CLIENT_ID": " cid ", "YOUTUBE_CLIENT_SECRET": "sec"}))
	if err != nil {
		t.Fatal(err)
	}
	y := cfg.YouTube
	if !cfg.Secrets.HasYouTube() || cfg.Secrets.YouTubeClientID != "cid" || y.Privacy != "public" || y.CategoryID != "20" ||
		!y.NotifySubscribers || y.MadeForKids || y.TokenFile != "/data/youtube_token.json" || len(y.Tags) == 0 || y.TitleHashtags[0] != "#GTA6" {
		t.Fatalf("youtube = %+v", y)
	}
	if cfg.Editor.MaxTokens != 16000 {
		t.Fatalf("editor.max_tokens default = %d", cfg.Editor.MaxTokens)
	}
	cfg, err = Parse([]byte("youtube: {privacy: unlisted, notify_subscribers: false, tags: ['GTA 6 Online']}"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets.HasYouTube() || cfg.YouTube.Privacy != "unlisted" || cfg.YouTube.NotifySubscribers || len(cfg.YouTube.Tags) != 1 {
		t.Fatalf("youtube = %+v", cfg.YouTube)
	}
}
