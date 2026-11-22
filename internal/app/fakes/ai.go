package fakes

import (
	"context"
	"sync"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Transcriber returns a fixed transcript (or a per-language one) and records
// the languages asked for.
type Transcriber struct {
	mu         sync.Mutex
	Transcript domain.Transcript
	Err        error
	Calls      int
	// ByLanguage overrides Transcript/Err for a language.
	ByLanguage map[string]TranscriptAnswer
	Languages  []string
}

// TranscriptAnswer is a scripted answer for one language.
type TranscriptAnswer struct {
	Transcript domain.Transcript
	Err        error
}

var _ app.Transcriber = (*Transcriber)(nil)

// Transcribe returns the scripted transcript or error.
func (t *Transcriber) Transcribe(ctx context.Context, _ domain.MediaRef, language string) (domain.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return domain.Transcript{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Calls++
	t.Languages = append(t.Languages, language)
	if a, ok := t.ByLanguage[language]; ok {
		return a.Transcript, a.Err
	}
	return t.Transcript, t.Err
}

// EditorAnswer is one scripted editor reply.
type EditorAnswer struct {
	Proposal domain.EditProposal
	Usage    domain.Usage
	Err      error
}

// Editor replays scripted answers (the last one repeats) and records requests.
type Editor struct {
	mu       sync.Mutex
	Answers  []EditorAnswer
	Requests []domain.EditRequest
}

var _ app.Editor = (*Editor)(nil)

// Plan returns the next scripted answer.
func (e *Editor) Plan(ctx context.Context, req domain.EditRequest) (domain.EditProposal, domain.Usage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EditProposal{}, domain.Usage{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Requests = append(e.Requests, req)
	i := min(len(e.Requests)-1, len(e.Answers)-1)
	if i < 0 {
		return domain.EditProposal{}, domain.Usage{}, nil
	}
	a := e.Answers[i]
	return a.Proposal, a.Usage, a.Err
}
