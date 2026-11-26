package main

import (
	"context"
	"errors"
	"flag"
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
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/clock"
	"github.com/henriquecpp/gta6-clipper/internal/platform/logging"
)

// offlineCut is the target length of the dry-run cut (clamped to the render limits).
const offlineCut = 30 * time.Second

// dryRun validates whisper and the final render with a real retained clip:
//
//	clipper dryrun                  list the clips that still have retained media
//	clipper dryrun CLIP_ID          transcribe, build a plan without AI, render
//	clipper dryrun -editor CLIP_ID  same, with the real editor (Claude: costs tokens)
//
// Without -editor the plan is deterministic (cut around the peak, captions
// with the original speech, no translation). The clip state is not changed
// and nothing is sent: the plan is printed and the video only goes to the
// output directory.
func dryRun(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dryrun", flag.ContinueOnError)
	useEditor := fs.Bool("editor", false, "plan with the real editor (Claude, costs tokens) instead of the offline plan")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) > 1 {
		return errors.New("usage: clipper dryrun [-editor] [CLIP_ID]")
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
	rend := render.New(ctx, render.Config{
		Encoder:        cfg.Render.Encoder,
		Threads:        cfg.Concurrency.FFmpegThreadsPerJob,
		PreviewWorkers: 1,
		FinalWorkers:   1,
		OutputDir:      cfg.Render.OutputDir,
		Style:          render.ASSStyle{Font: cfg.Render.Font, MaxLineChars: cfg.Render.MaxLineChars},
	}, render.WithLogger(log))

	var plan domain.EditPlan
	if *useEditor {
		// Same editor path as production (frames, game-language pass, one
		// retry), nothing saved; the notifier and media are never used.
		produce := app.NewProduceFinalClip(pc, repo, tr, newEditor(cfg, log), rend, nil, nil, clock.Real{}, log, nil, nil,
			app.WithFrameSampler(rend))
		t2 := time.Now()
		var usage domain.Usage
		plan, usage, err = produce.DraftPlan(ctx, c, transcript)
		if err != nil {
			return fmt.Errorf("editor plan: %w", err)
		}
		printPlan(out, plan, usage, pc.Pricing, time.Since(t2))
	} else {
		plan, err = domain.NewEditPlan(offlineProposal(transcript, cons), cons)
		if err != nil {
			return fmt.Errorf("offline plan: %w", err)
		}
		plan = plan.SnapToWords(transcript, pc.SnapMargin, cons)
	}
	_, _ = fmt.Fprintf(out, "cut: %.2fs–%.2fs (peak at %.2fs, window %.1fs), %d captions\n", plan.Start.Seconds(),
		plan.End.Seconds(), cons.Peak.Seconds(), cons.WindowDuration.Seconds(), len(plan.Captions))
	// The clip's framing (decided at preview time), or a fresh one (auto layout:
	// facecam detection) for clips that never had a preview.
	layout, stack := c.Layout, c.Stack
	if !layout.Valid() || layout == domain.LayoutAuto {
		layout, stack = app.NewFraming(newFacecamDetector(cfg, log), pc.Streamers, clock.Real{}, log, nil).Resolve(ctx, c)
	}
	_, _ = fmt.Fprintf(out, "framing: %s %+v\n", layout, stack.Facecam)
	t1 := time.Now()
	video, err := rend.Final(ctx, c.SourceMedia, domain.RenderSpec{
		ClipID: c.ID, Streamer: c.Streamer, Start: plan.Start, End: plan.End,
		Width: pc.Width, Height: pc.Height, FPS: pc.FPS, Layout: layout, Stack: stack,
		Headline: plan.Headline, Captions: plan.Captions, Words: transcript.Between(plan.Start, plan.End), MaxBytes: pc.MaxBytes,
	})
	if err != nil {
		return fmt.Errorf("render final: %w", err)
	}
	_, _ = fmt.Fprintf(out, "final video (%s, %s): %s\n", rend.Encoder(), time.Since(t1).Round(time.Millisecond), video)
	return nil
}

// printPlan shows the editor's texts and cost.
func printPlan(out io.Writer, p domain.EditPlan, u domain.Usage, pricing domain.Pricing, took time.Duration) {
	_, _ = fmt.Fprintf(out, "editor (%s, %s; %d in, %d out, %d cache write, %d cache read):\n", u.Cost(pricing),
		took.Round(time.Millisecond), u.InputTokens, u.OutputTokens, u.CacheWriteTokens, u.CacheReadTokens)
	_, _ = fmt.Fprintf(out, "  headline:      %s\n  youtube_title: %s\n  post_text:     %s\n  search_tags:   %s\n  reasoning:     %s\n",
		p.Headline, p.YouTubeTitle, p.PostText, strings.Join(p.SearchTags, " | "), p.Reasoning)
	for _, c := range p.Captions {
		_, _ = fmt.Fprintf(out, "  %6.2f–%6.2f  %s\n", c.Start.Seconds(), c.End.Seconds(), c.Text)
	}
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
