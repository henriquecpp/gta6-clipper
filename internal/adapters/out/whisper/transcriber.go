package whisper

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/proc"
)

// Transcriber implements app.Transcriber: it extracts 16 kHz mono WAV from the
// retained media with ffmpeg and sends it to whisper-server. A semaphore keeps
// the GPU from being shared by more than `workers` transcriptions.
type Transcriber struct {
	client  *Client
	sem     chan struct{}
	timeout time.Duration
	source  TimestampSource
	extract func(ctx context.Context, src string) ([]byte, error)
}

var _ app.Transcriber = (*Transcriber)(nil)

// TranscriberOption configures a Transcriber.
type TranscriberOption func(*Transcriber)

// WithWorkers bounds concurrent transcriptions (default 1).
func WithWorkers(n int) TranscriberOption {
	return func(t *Transcriber) { t.sem = make(chan struct{}, max(n, 1)) }
}

// WithTimeout bounds one transcription.
func WithTimeout(d time.Duration) TranscriberOption { return func(t *Transcriber) { t.timeout = d } }

// WithTimestampSource selects token or DTW word starts (default DTW, ADR 0012).
func WithTimestampSource(s TimestampSource) TranscriberOption {
	return func(t *Transcriber) { t.source = s }
}

// withExtractor replaces the ffmpeg audio extraction (tests).
func withExtractor(f func(ctx context.Context, src string) ([]byte, error)) TranscriberOption {
	return func(t *Transcriber) { t.extract = f }
}

// NewTranscriber builds the transcriber.
func NewTranscriber(c *Client, opts ...TranscriberOption) *Transcriber {
	t := &Transcriber{client: c, sem: make(chan struct{}, 1), timeout: 5 * time.Minute, source: TimestampsDTW}
	t.extract = func(ctx context.Context, src string) ([]byte, error) {
		return proc.Run(ctx, nil, "ffmpeg", extractArgs(src)...)
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// extractArgs decodes the audio track to 16 kHz mono 16-bit WAV on stdout.
// aresample pads the start and fills timestamp gaps with silence, so word
// times stay on the timeline the renderer cuts (relative to the file start).
func extractArgs(src string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-i", src, "-vn", "-af", "aresample=async=1:first_pts=0", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", "pipe:1"}
}

// Transcribe returns the words of the media with times relative to its start.
func (t *Transcriber) Transcribe(ctx context.Context, src domain.MediaRef) (domain.Transcript, error) {
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		return domain.Transcript{}, ctx.Err()
	}
	defer func() { <-t.sem }()
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	wav, err := t.extract(ctx, string(src))
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("extract audio of %s: %w", src, err)
	}
	body, err := t.client.Inference(ctx, bytes.NewReader(wav), InferenceOptions{Language: "auto"})
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("transcribe %s: %w", src, err)
	}
	return ParseVerboseJSON(body, t.source)
}
