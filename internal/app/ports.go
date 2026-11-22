// Package app holds the use cases and the ports (interfaces) they need. Ports
// are defined here, by the consumer, and kept small. The package depends only
// on the domain and the standard library (enforced by depguard).
package app

import (
	"context"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Platform is the streaming platform (Twitch Helix).
type Platform interface {
	// LiveStreams returns the broadcasts currently on air among logins.
	LiveStreams(ctx context.Context, logins []string) ([]domain.LiveStream, error)
	// RecentClips returns viewer clips of a broadcaster created since the given time.
	RecentClips(ctx context.Context, broadcasterID string, since time.Time) ([]domain.ViewerClip, error)
}

// ClipRepository persists clips. Every state transition happens inside InTx
// (read, validate, write), which makes duplicate clicks idempotent.
type ClipRepository interface {
	Save(ctx context.Context, c *domain.Clip) error
	// Get returns domain.ErrNotFound when the clip does not exist.
	Get(ctx context.Context, id domain.ClipID) (*domain.Clip, error)
	// ListByState returns the clips in any of the states, oldest first.
	ListByState(ctx context.Context, states ...domain.ClipState) ([]*domain.Clip, error)
	// InTx runs fn in a transaction; fn must use the repository it receives.
	// An error returned by fn rolls the transaction back and is returned as is.
	InTx(ctx context.Context, fn func(ctx context.Context, repo ClipRepository) error) error
}

// Clock is the source of the current time; domain and app never call time.Now.
type Clock interface {
	Now() time.Time
}

// LiveObserver is told when a monitored streamer goes live or offline. The
// ingestion supervisor implements it to start and stop per-stream capture.
// Implementations must return quickly.
type LiveObserver interface {
	StreamStarted(ctx context.Context, s domain.Streamer, live domain.LiveStream)
	// StreamUpdated reports refreshed metadata (viewers, title, game) of a live.
	StreamUpdated(ctx context.Context, live domain.LiveStream)
	StreamEnded(ctx context.Context, s domain.Streamer, live domain.LiveStream)
}

// MediaCapture holds the circular buffer of the captured streams.
type MediaCapture interface {
	// RetainWindow copies (without re-encoding) the window [from, to] of the
	// buffer to the pending storage and returns the window actually retained
	// (whole segments, so it may be slightly larger). It returns
	// domain.ErrWindowUnavailable if the window is no longer (or not yet) in
	// the buffer.
	RetainWindow(ctx context.Context, streamID domain.StreamID, from, to domain.StreamPos) (domain.MediaRef, domain.Window, error)
	// Delete removes a retained file; deleting a missing file is not an error.
	Delete(ctx context.Context, ref domain.MediaRef) error
}

// Renderer produces videos from retained media (ffmpeg + NVENC).
type Renderer interface {
	// Preview renders a short low-resolution preview next to the source.
	Preview(ctx context.Context, src domain.MediaRef, spec domain.PreviewSpec) (domain.MediaRef, error)
	// Final renders the vertical video with burned-in subtitles.
	Final(ctx context.Context, src domain.MediaRef, spec domain.RenderSpec) (domain.MediaRef, error)
}

// Transcriber transcribes retained media locally (whisper.cpp on the GPU).
// language is the ISO code of the speech to transcribe ("" = detect). Forcing
// it matters: detection looks only at the start of the audio, and a game
// cutscene in English there turns Portuguese speech into made-up English.
type Transcriber interface {
	Transcribe(ctx context.Context, src domain.MediaRef, language string) (domain.Transcript, error)
}

// Editor asks the model for the cut, headline, captions and post text. It
// returns the raw proposal; validation is a domain rule. A malformed answer
// (no tool call, invalid JSON) is reported as domain.ErrInvalidEditPlan.
type Editor interface {
	Plan(ctx context.Context, req domain.EditRequest) (domain.EditProposal, domain.Usage, error)
}

// Notifier talks to the human approver (Telegram).
type Notifier interface {
	// SendPreview sends the preview with Approve/Discard buttons and returns
	// a reference to the message(s), used later to edit them.
	SendPreview(ctx context.Context, c *domain.Clip, preview domain.MediaRef) (domain.MessageRef, error)
	// UpdatePreview edits the preview message(s) to reflect the clip state.
	UpdatePreview(ctx context.Context, c *domain.Clip) error
	// SendFailure alerts about a failed clip.
	SendFailure(ctx context.Context, c *domain.Clip, reason string) error
	// SendFinal delivers the final video and the suggested post text and
	// returns a reference to the video message(s), edited by UpdateFinal.
	SendFinal(ctx context.Context, c *domain.Clip, video domain.MediaRef, post domain.Post) (domain.MessageRef, error)
	// UpdateFinal edits the final video message(s) to reflect the publication.
	UpdateFinal(ctx context.Context, c *domain.Clip) error
	// SendAlert sends an informational alert about a clip (e.g. cost above the limit).
	SendAlert(ctx context.Context, c *domain.Clip, text string) error
}

// Publisher uploads a delivered video to the channel (YouTube Shorts). It runs
// only after the human asks for it. An upload is not idempotent, so callers
// must not retry a failed call blindly (it could publish the video twice).
type Publisher interface {
	Publish(ctx context.Context, video domain.MediaRef, meta domain.UploadMeta) (domain.Publication, error)
}
