package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"slices"
	"syscall"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite"
	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/youtube"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// youtubeUpdate rewrites the title, description and tags of Shorts already
// published, with the current youtube settings (hashtags, description line):
//
//	clipper youtube-update                   show what would change (reads only)
//	clipper youtube-update -apply            update every published Short
//	clipper youtube-update [-apply] CLIP_ID  only these clips
//
// The texts come from the clip as saved (editor's title, post text, search
// tags), so no AI is called. Each update costs 50 units of the YouTube quota.
func youtubeUpdate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtube-update", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "update the videos on YouTube (without it, only show the changes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !cfg.Secrets.HasYouTube() {
		return errors.New("YOUTUBE_CLIENT_ID / YOUTUBE_CLIENT_SECRET are not set (.env)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // read-only use; nothing to flush
	repo := sqlite.NewRepo(db)

	clips, err := repo.ListByState(ctx, domain.StatePublished)
	if err != nil {
		return err
	}
	if ids := fs.Args(); len(ids) > 0 {
		clips = slices.DeleteFunc(clips, func(c *domain.Clip) bool { return !slices.Contains(ids, string(c.ID)) })
	}
	clips = slices.DeleteFunc(clips, func(c *domain.Clip) bool { return c.PublishedID == "" })
	if len(clips) == 0 {
		_, _ = fmt.Fprintln(out, "Nenhum Short publicado para atualizar.")
		return nil
	}

	yt := newYouTube(cfg, slog.New(slog.DiscardHandler))
	if *apply {
		ok, err := yt.CanEdit()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("a autorização atual do YouTube só permite enviar vídeos, não editar: rode `task auth:youtube` " +
				"de novo (agora pede também a permissão de editar), reinicie o app e repita")
		}
	}
	opts := uploadOptions(cfg)
	streamers := streamerMap(cfg)
	changed, updated := 0, 0
	for _, c := range clips {
		o := opts
		o.AudioLanguage = streamers[c.Streamer].Language
		meta := domain.BuildUploadMeta(c, o)
		cur, err := yt.Video(ctx, c.PublishedID)
		if err != nil {
			return err
		}
		if !textChanged(cur, meta) {
			_, _ = fmt.Fprintf(out, "= %s (%s) já está atualizado\n", c.PublishedURL, c.Streamer)
			continue
		}
		changed++
		_, _ = fmt.Fprintf(out, "\n▶ %s (%s, clipe %s)\n", c.PublishedURL, c.Streamer, c.ID)
		printChange(out, "Título", cur.Title, meta.Title)
		printChange(out, "Descrição", cur.Description, meta.Description)
		_, _ = fmt.Fprintf(out, "  Tags: %d → %d\n", len(cur.Tags), len(meta.Tags))
		if !*apply {
			continue
		}
		if _, err := yt.UpdateText(ctx, c.PublishedID, cur, meta); err != nil {
			return fmt.Errorf("%d de %d atualizados; parou em %s: %w", updated, changed, c.PublishedURL, err)
		}
		updated++
		_, _ = fmt.Fprintln(out, "  ✅ atualizado")
	}
	switch {
	case changed == 0:
		_, _ = fmt.Fprintln(out, "\nNada a mudar.")
	case *apply:
		_, _ = fmt.Fprintf(out, "\n%d Short(s) atualizados (%d unidades de cota).\n", updated, 50*updated)
	default:
		_, _ = fmt.Fprintf(out, "\n%d Short(s) mudariam. Nada foi alterado: rode com -apply para atualizar (50 unidades de cota cada).\n", changed)
	}
	return nil
}

// textChanged reports whether the video's title, description or tags differ
// from what the current settings produce.
func textChanged(cur youtube.Snippet, meta domain.UploadMeta) bool {
	return cur.Title != meta.Title || cur.Description != meta.Description || !slices.Equal(cur.Tags, meta.Tags)
}

func printChange(out io.Writer, field, before, after string) {
	if before == after {
		_, _ = fmt.Fprintf(out, "  %s: (igual)\n", field)
		return
	}
	_, _ = fmt.Fprintf(out, "  %s antes:\n    %q\n  %s depois:\n    %q\n", field, before, field, after)
}
