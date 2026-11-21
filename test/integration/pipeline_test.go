//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/mymmrac/telego"
	"golang.org/x/sync/errgroup"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/telegrambot"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/in/twitchingest"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/capture"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/claude"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/render"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/telegram"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/whisper"
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/domain/detect"
	"github.com/henriquecpp/gta6-clipper/internal/platform/clock"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
	"github.com/henriquecpp/gta6-clipper/internal/platform/telegramtest"
)

const (
	sampleVideo = "../../testdata/video/sample.mp4"
	chatID      = int64(111)
)

// whisperJSON is a canned whisper-server verbose_json answer.
const whisperJSON = `{"language":"english","language_probabilities":{"en":0.97},"segments":[{"text":" No way did you see that","start":1.0,"end":3.0,"words":[
{"word":" No","start":1.00,"end":1.20,"t_dtw":105,"probability":0.9},
{"word":" way","start":1.25,"end":1.60,"t_dtw":130,"probability":0.9},
{"word":" did","start":1.70,"end":1.90,"t_dtw":172,"probability":0.9},
{"word":" you","start":1.95,"end":2.10,"t_dtw":197,"probability":0.9},
{"word":" see","start":2.15,"end":2.50,"t_dtw":218,"probability":0.9},
{"word":" that","start":2.55,"end":3.00,"t_dtw":258,"probability":0.9}]}]}`

// claudeReply is a canned Messages API answer with a valid edit plan for a
// ~16 s window whose peak is at ~10 s.
const claudeReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","stop_sequence":null,
"usage":{"input_tokens":3000,"output_tokens":400,"cache_creation_input_tokens":1500,"cache_read_input_tokens":0},
"content":[{"type":"tool_use","id":"toolu_1","name":"submit_edit_plan","input":{"start_s":0.8,"end_s":11,
"headline":"Ninguém esperava esse pulo!","captions":[{"start_s":1,"end_s":3,"text":"Não acredito, você viu isso?"}],
"post_text":"Que momento! Clipe da live de sample #gta6 #twitch","reasoning":"setup curto e reação"}}]}`

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func probe(t *testing.T, path string) (w, h int, dur float64) {
	t.Helper()
	out, err := proc.Run(context.Background(), nil, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration", "-of", "default=nw=1", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "width":
			w, _ = strconv.Atoi(v)
		case "height":
			h, _ = strconv.Atoi(v)
		case "duration":
			dur, _ = strconv.ParseFloat(v, 64)
		}
	}
	return w, h, dur
}

// TestPipelineEndToEnd: a live starts → capture fills the buffer → viewer
// clips create a (late) moment → window retained → preview on Telegram →
// approval by button → transcription → Claude → render → delivery.
func TestPipelineEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dir := t.TempDir()
	clk := clock.Real{}

	db, err := sqlite.Open(ctx, filepath.Join(dir, "clipper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := sqlite.NewRepo(db)

	capt, err := capture.New(capture.Config{
		BufferDir: filepath.Join(dir, "buffer"), PendingDir: filepath.Join(dir, "pending"),
		BufferDuration: 5 * time.Minute, MaxBytes: 1 << 30, SegmentDuration: 2 * time.Second,
		Frame: 30 * time.Millisecond, Threads: 2, Workers: 1,
	}, capture.WithLogger(log), capture.WithSource(func(ctx context.Context, _ domain.LiveStream) *exec.Cmd {
		// The sample replayed in real time stands in for the Twitch stream.
		return proc.Command(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-i", sampleVideo, "-c", "copy", "-f", "mpegts", "pipe:1")
	}))
	if err != nil {
		t.Fatal(err)
	}
	rend := render.New(ctx, render.Config{Encoder: render.EncoderX264, Threads: 4, OutputDir: filepath.Join(dir, "output"),
		Style: render.ASSStyle{Font: "Noto Sans", MaxLineChars: 28}}, render.WithLogger(log))

	tg := telegramtest.New()
	defer tg.Close()
	bot, err := telego.NewBot(telegramtest.Token, telego.WithAPIServer(tg.URL), telego.WithHTTPClient(tg.Client()), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	notifier := telegram.NewNotifier(bot, []int64{chatID}, time.Hour, log)

	var whisperCalls, claudeCalls atomic.Int32
	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		whisperCalls.Add(1)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, fh, err := r.FormFile("file")
		if err != nil || fh.Size < 100_000 { // ~16 s of 16 kHz mono WAV ≈ 500 KB
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = f.Close()
		_, _ = io.WriteString(w, whisperJSON)
	}))
	defer whisperSrv.Close()
	claudeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeReply)
	}))
	defer claudeSrv.Close()

	idN := 0
	newID := func() (domain.ClipID, error) {
		idN++
		return domain.ClipID(fmt.Sprintf("0193f1c2-0000-7000-8000-%012d", idN)), nil
	}
	errLog := &app.ErrorLog{}
	sendPreview := app.NewSendPreview(app.PreviewConfig{Duration: 40 * time.Second, Before: 25 * time.Second, Width: 480, Height: 854, FPS: 30, MaxBytes: 45 << 20},
		repo, rend, notifier, clk, log, nil, errLog)
	detectMoment := app.NewDetectMoment(app.DetectionConfig{
		Tick: 500 * time.Millisecond,
		// The sample video has no voice/chat reaction, so the reaction gate is
		// off here (MinSignalZ 0; the warmup already disables the fast path).
		// The gate itself is covered by the app unit tests.
		Scorer: detect.ScorerConfig{
			Weights:     map[domain.SignalKind]float64{domain.SignalVoice: 0.45, domain.SignalChat: 0.45, domain.SignalClips: 0.10},
			ThresholdOn: 3, ThresholdOff: 1.5, ZMax: 6, MinSignalZ: 0, Warmup: 10 * time.Minute, Cooldown: time.Minute,
		},
		Voice: detect.VoiceConfig{Window: 400 * time.Millisecond, Alpha: 0.02}, Chat: detect.ChatConfig{Window: 10 * time.Second, Sustain: 3 * time.Second, Alpha: 0.02},
		Clips:   detect.ClipsConfig{GroupWindow: 15 * time.Second, MinClips: 3},
		PreRoll: 10 * time.Second, PostRoll: 5 * time.Second, BufferDuration: 5 * time.Minute, RetainMargin: 3 * time.Second,
	}, repo, newID, clk, log, app.WithCapture(capt), app.WithClipHandler(func(ctx context.Context, c *domain.Clip) {
		if err := sendPreview.Execute(ctx, c.ID); err != nil {
			t.Errorf("preview: %v", err)
		}
	}))
	produce := app.NewProduceFinalClip(app.ProduceConfig{
		MinDuration: 5 * time.Second, MaxDuration: 15 * time.Second, PeakTolerance: 3 * time.Second, HeadlineMaxChars: 60,
		SnapMargin: 150 * time.Millisecond, Pricing: domain.Pricing{Input: 100_000, Output: 500_000, CacheWrite: 125_000, CacheRead: 10_000},
		MaxCost: 50_000, Width: 1080, Height: 1920, FPS: 30, MaxBytes: 45 << 20,
		Streamers: map[string]domain.Streamer{"sample": {Login: "sample", Layout: domain.LayoutBlur}},
	}, repo, whisper.NewTranscriber(whisper.NewClient(whisperSrv.URL, nil)),
		claude.New(claude.Config{APIKey: "sk-test", Model: "claude-haiku-5-5", MaxTokens: 2000, Timeout: 30 * time.Second}, option.WithBaseURL(claudeSrv.URL)),
		rend, notifier, capt, clk, log, nil, errLog)
	queue := app.NewProduceQueue(produce.Execute, 1, 10, 30*time.Second, log, nil)
	decisions := app.NewDecisions(repo, capt, notifier, clk, log, time.Hour,
		app.WithProducer(func(ctx context.Context, id domain.ClipID) { queue.Submit(ctx, id) }))
	status := app.NewStatus(repo, clk, time.Hour, nil, queue.Depth, errLog)
	tgBot := telegrambot.New(bot, []int64{chatID}, decisions, status, log)

	start := time.Now().UTC()
	live := domain.LiveStream{ID: "s1", BroadcasterID: "b1", Login: "sample", Game: "Grand Theft Auto VI", Title: "teste", Viewers: 500, StartedAt: start}
	streamer := domain.Streamer{Login: "sample", Language: "en", AllowsClips: true, Layout: domain.LayoutBlur}
	platform := &fakes.Platform{Streams: []domain.LiveStream{live}, Clips: map[string][]domain.ViewerClip{"b1": {
		// Three viewer clips whose VOD offset puts the moment at ~20 s of the capture.
		{ID: "v1", CreatedAt: start, Duration: 10 * time.Second, VODOffset: 15 * time.Second, HasVODOffset: true},
		{ID: "v2", CreatedAt: start, Duration: 10 * time.Second, VODOffset: 14 * time.Second, HasVODOffset: true},
		{ID: "v3", CreatedAt: start, Duration: 10 * time.Second, VODOffset: 16 * time.Second, HasVODOffset: true},
	}}}
	sup := twitchingest.NewSupervisor(twitchingest.SupervisorConfig{Tick: 500 * time.Millisecond, ClipPollInterval: 2 * time.Second,
		ClipLookback: 10 * time.Minute, MaxStreams: 2}, detectMoment, platform, log, twitchingest.WithCapturer(capt))
	monitor := app.NewMonitorLives(platform, []domain.Streamer{streamer}, nil, sup, log)

	runCtx, stop := context.WithCancel(ctx)
	g, gctx := errgroup.WithContext(runCtx)
	g.Go(func() error { return sup.Run(gctx) })
	g.Go(func() error { return queue.Run(gctx) })
	g.Go(func() error { return tgBot.Run(gctx) })
	if err := monitor.Poll(ctx); err != nil {
		t.Fatal(err)
	}

	var clip *domain.Clip
	waitFor(t, "preview sent", 90*time.Second, func() bool {
		cs, _ := repo.ListByState(ctx, domain.StatePreviewSent)
		if len(cs) > 0 {
			clip = cs[0]
		}
		return clip != nil
	})
	if !clip.Moment.Late || clip.Moment.ViewerClips != 3 || clip.Window.Duration() < 14*time.Second {
		t.Fatalf("clip = %+v window %v", clip.Moment, clip.Window)
	}
	previews := tg.Calls("sendVideo")
	if len(previews) != 1 || !strings.Contains(previews[0].Params["reply_markup"], "a:"+string(clip.ID)) || previews[0].FileBytes == 0 {
		t.Fatalf("preview message = %+v", previews)
	}
	if w, h, _ := probe(t, string(clip.PreviewMedia)); w != 480 || h != 854 {
		t.Fatalf("preview %dx%d", w, h)
	}

	// The human taps "Aprovar" (twice: the second tap must be harmless).
	cb := func(id string) map[string]any {
		return map[string]any{"callback_query": map[string]any{"id": id, "data": "a:" + string(clip.ID),
			"from":    map[string]any{"id": chatID, "is_bot": false, "first_name": "me"},
			"message": map[string]any{"message_id": 101, "date": 1, "chat": map[string]any{"id": chatID, "type": "private"}}}}
	}
	tg.PushUpdate(cb("q1"))
	tg.PushUpdate(cb("q2"))

	waitFor(t, "clip delivered", 150*time.Second, func() bool {
		c, err := repo.Get(ctx, clip.ID)
		if err == nil && c.State == domain.StateFailed {
			t.Fatalf("clip failed: %s", c.FailureReason)
		}
		return err == nil && c.State == domain.StateDelivered
	})
	stop()
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}

	final, _ := repo.Get(ctx, clip.ID)
	if w, h, dur := probe(t, string(final.FinalMedia)); w != 1080 || h != 1920 || dur < 9 || dur > 11.5 {
		t.Fatalf("final video %dx%d %.2fs", w, h, dur)
	}
	if final.Headline != "Ninguém esperava esse pulo!" || final.Cost == 0 || final.Plan == nil || claudeCalls.Load() != 1 || whisperCalls.Load() != 1 {
		t.Fatalf("final clip = %+v (claude %d, whisper %d)", final, claudeCalls.Load(), whisperCalls.Load())
	}
	answers := tg.Calls("answerCallbackQuery")
	if len(answers) != 2 || answers[1].Params["text"] != "Já processado" {
		t.Fatalf("callback answers = %+v", answers)
	}
	videos := tg.Calls("sendVideo")
	msgs := tg.Calls("sendMessage")
	if len(videos) != 2 || len(msgs) != 1 || !strings.Contains(msgs[0].Params["text"], "sample") {
		t.Fatalf("delivery: videos=%d messages=%+v", len(videos), msgs)
	}
	edits := tg.Calls("editMessageCaption")
	if len(edits) < 2 || !strings.Contains(edits[len(edits)-1].Params["caption"], "Entregue") {
		t.Fatalf("preview edits = %+v", edits)
	}
	if _, err := os.Stat(string(final.SourceMedia)); !os.IsNotExist(err) {
		t.Fatal("retained media must be deleted after delivery")
	}
	t.Logf("delivered %s (%s), cost %s", final.FinalMedia, final.Headline, final.Cost)
}
