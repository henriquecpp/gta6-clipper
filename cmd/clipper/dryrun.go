package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/render"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/whisper"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/logging"
)

// offlineCut is the target length of the dry-run cut (clamped to the render limits).
const offlineCut = 30 * time.Second

// dryRun validates whisper and the final render with a real retained clip
// while the editor (Claude) is not configured:
//
//	clipper dryrun            list the clips that still have retained media
//	clipper dryrun CLIP_ID    transcribe, build a plan without AI, render
//
// The plan is deterministic (cut around the peak, captions with the original
// speech, no translation). The clip state is not changed and nothing is sent:
// the video only goes to the output directory.
func dryRun(args []string, out io.Writer) error {
	if len(args) > 1 {
		return errors.New("usage: clipper dryrun [CLIP_ID]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log, err := logging.New(os.Stderr, cfg.LogLevel, logging.FormatJSON)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // read-only use; nothing to flush
	repo := sqlite.NewRepo(db)

	if len(args) == 0 {
		clips, err := repo.ListByState(ctx, domain.StateDetected, domain.StatePreviewSent, domain.StateApproved,
			domain.StateEditing, domain.StateRendered, domain.StateFailed)
		if err != nil {
			return err
		}
		for _, c := range clips {
			if c.SourceMedia == "" {
				continue
			}
			_, _ = fmt.Fprintf(out, "%s  %-14s %-12s score %.1f  window %4.0fs  %s\n", c.ID, c.Streamer, c.State,
				c.Moment.Score, c.Window.Duration().Seconds(), c.CreatedAt.UTC().Format(time.DateTime))
		}
		return nil
	}

	c, err := repo.Get(ctx, domain.ClipID(args[0]))
	if err != nil {
		return err
	}
	if c.SourceMedia == "" {
		return fmt.Errorf("clip %s has no retained media (state %s)", c.ID, c.State)
	}

	tr := whisper.NewTranscriber(whisper.NewClient(cfg.Whisper.URL, &http.Client{Timeout: cfg.Whisper.Timeout.D()}),
		whisper.WithTimeout(cfg.Whisper.Timeout.D()), whisper.WithBeamSize(cfg.Whisper.BeamSize))
	t0 := time.Now()
	transcript, err := tr.Transcribe(ctx, c.SourceMedia, streamerMap(cfg)[c.Streamer].Language)
	if err != nil {
		return fmt.Errorf("transcribe: %w", err)
	}
	_, _ = fmt.Fprintf(out, "transcript: language %s, %d words, %s\n%s\n\n", transcript.Language, len(transcript.Words),
		time.Since(t0).Round(time.Millisecond), transcript.Text())

	pc := produceConfig(cfg)
	cons := domain.EditConstraints{
		WindowDuration:   c.Window.Duration(),
		Peak:             time.Duration(c.Moment.Peak - c.Window.From),
		MinDuration:      pc.MinDuration,
		MaxDuration:      pc.MaxDuration,
		PeakTolerance:    pc.PeakTolerance,
		HeadlineMaxChars: pc.HeadlineMaxChars,
		Streamer:         c.Streamer,
	}
	plan, err := domain.NewEditPlan(offlineProposal(transcript, cons), cons)
	if err != nil {
		return fmt.Errorf("offline plan: %w", err)
	}
	plan = plan.SnapToWords(transcript, pc.SnapMargin, cons)
	_, _ = fmt.Fprintf(out, "cut: %.2fs–%.2fs (peak at %.2fs, window %.1fs), %d captions\n", plan.Start.Seconds(),
		plan.End.Seconds(), cons.Peak.Seconds(), cons.WindowDuration.Seconds(), len(plan.Captions))

	rend := render.New(ctx, render.Config{
		Encoder:        cfg.Render.Encoder,
		Threads:        cfg.Concurrency.FFmpegThreadsPerJob,
		PreviewWorkers: 1,
		FinalWorkers:   1,
		OutputDir:      cfg.Render.OutputDir,
		Style:          render.ASSStyle{Font: cfg.Render.Font, MaxLineChars: cfg.Render.MaxLineChars},
	}, render.WithLogger(log))
	st := pc.Streamers[c.Streamer]
	layout := st.Layout
	if !layout.Valid() {
		layout = domain.LayoutBlur
	}
	t1 := time.Now()
	video, err := rend.Final(ctx, c.SourceMedia, domain.RenderSpec{
		ClipID: c.ID, Streamer: c.Streamer, Start: plan.Start, End: plan.End,
		Width: pc.Width, Height: pc.Height, FPS: pc.FPS, Layout: layout, Stack: st.Stack,
		Headline: plan.Headline, Captions: plan.Captions, Words: transcript.Between(plan.Start, plan.End), MaxBytes: pc.MaxBytes,
	})
	if err != nil {
		return fmt.Errorf("render final: %w", err)
	}
	_, _ = fmt.Fprintf(out, "final video (%s, %s): %s\n", rend.Encoder(), time.Since(t1).Round(time.Millisecond), video)
	return nil
}

// offlineProposal builds a deterministic edit without AI: a cut of about
// offlineCut with the peak at 40% of it (most of the cut is the reaction),
// and captions made of the transcribed words (up to 5 words or 3 s each, not
// overlapping).
func offlineProposal(t domain.Transcript, c domain.EditConstraints) domain.EditProposal {
	length := min(max(offlineCut, c.MinDuration), c.MaxDuration, c.WindowDuration)
	start := max(0, min(c.Peak-length*2/5, c.WindowDuration-length))
	end := start + length

	var caps []domain.ProposedCaption
	var group []string
	var gStart, gEnd, prevEnd time.Duration
	flush := func() {
		if len(group) == 0 {
			return
		}
		cs, ce := max(gStart, start, prevEnd), min(gEnd, end)
		if cs < ce {
			caps = append(caps, domain.ProposedCaption{StartS: cs.Seconds(), EndS: ce.Seconds(), Text: strings.Join(group, " ")})
			prevEnd = ce
		}
		group = group[:0]
	}
	for _, w := range t.Between(start, end) {
		text := strings.TrimSpace(w.Text)
		if text == "" {
			continue
		}
		if len(group) > 0 && (len(group) >= 5 || w.End-gStart > 3*time.Second) {
			flush()
		}
		if len(group) == 0 {
			gStart = w.Start
		}
		group = append(group, text)
		gEnd = w.End
	}
	flush()
	if len(caps) == 0 {
		caps = []domain.ProposedCaption{{StartS: start.Seconds(), EndS: end.Seconds(), Text: "(sem fala detectada)"}}
	}

	headline := "Teste sem IA — " + c.Streamer
	if c.HeadlineMaxChars > 0 && utf8.RuneCountInString(headline) > c.HeadlineMaxChars {
		headline = string([]rune(headline)[:c.HeadlineMaxChars])
	}
	return domain.EditProposal{
		StartS:    start.Seconds(),
		EndS:      end.Seconds(),
		Headline:  headline,
		Captions:  caps,
		PostText:  "Teste de render sem IA. Créditos: twitch.tv/" + c.Streamer,
		Reasoning: "dry run: cut around the peak, original speech",
	}
}
