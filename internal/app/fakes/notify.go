package fakes

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Renderer is a scripted Renderer.
type Renderer struct {
	mu         sync.Mutex
	PreviewErr error
	FinalErr   error
	Previews   []domain.PreviewSpec
	Finals     []domain.RenderSpec
	// Media, when set, registers produced files as existing.
	Media *MediaCapture
}

var _ app.Renderer = (*Renderer)(nil)

// Preview returns <src>.preview.mp4.
func (r *Renderer) Preview(ctx context.Context, src domain.MediaRef, spec domain.PreviewSpec) (domain.MediaRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.PreviewErr != nil {
		return "", r.PreviewErr
	}
	r.Previews = append(r.Previews, spec)
	ref := domain.MediaRef(strings.TrimSuffix(string(src), ".ts") + ".preview.mp4")
	if r.Media != nil {
		r.Media.Add(ref)
	}
	return ref, nil
}

// Final returns /output/<clip>.mp4.
func (r *Renderer) Final(ctx context.Context, _ domain.MediaRef, spec domain.RenderSpec) (domain.MediaRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FinalErr != nil {
		return "", r.FinalErr
	}
	r.Finals = append(r.Finals, spec)
	return domain.MediaRef("/output/" + string(spec.ClipID) + ".mp4"), nil
}

// Notifier records what would be sent to Telegram.
type Notifier struct {
	mu        sync.Mutex
	SendErr   error
	UpdateErr error
	FinalErr  error
	Sent      []domain.ClipID
	Updates   []domain.ClipState
	Failures  []string
	Finals    []domain.Post
	Alerts    []string
	// FinalUpdates records the clip states shown on the final message.
	FinalUpdates []domain.ClipState
}

var _ app.Notifier = (*Notifier)(nil)

// SendPreview records the preview.
func (n *Notifier) SendPreview(ctx context.Context, c *domain.Clip, _ domain.MediaRef) (domain.MessageRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.SendErr != nil {
		return "", n.SendErr
	}
	n.Sent = append(n.Sent, c.ID)
	return domain.MessageRef(fmt.Sprintf("1:%d", len(n.Sent))), nil
}

// UpdatePreview records the state shown.
func (n *Notifier) UpdatePreview(_ context.Context, c *domain.Clip) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Updates = append(n.Updates, c.State)
	return n.UpdateErr
}

// SendFailure records the alert.
func (n *Notifier) SendFailure(_ context.Context, c *domain.Clip, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Failures = append(n.Failures, string(c.ID)+": "+reason)
	return nil
}

// SendFinal records the delivery.
func (n *Notifier) SendFinal(ctx context.Context, _ *domain.Clip, _ domain.MediaRef, post domain.Post) (domain.MessageRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.FinalErr != nil {
		return "", n.FinalErr
	}
	n.Finals = append(n.Finals, post)
	return domain.MessageRef(fmt.Sprintf("1:%d", 1000+len(n.Finals))), nil
}

// UpdateFinal records the state shown on the final message.
func (n *Notifier) UpdateFinal(_ context.Context, c *domain.Clip) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.FinalUpdates = append(n.FinalUpdates, c.State)
	return n.UpdateErr
}

// FinalStates returns a copy of the states shown on the final message.
func (n *Notifier) FinalStates() []domain.ClipState {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]domain.ClipState(nil), n.FinalUpdates...)
}

// Publisher records uploads (YouTube).
type Publisher struct {
	mu      sync.Mutex
	Err     error
	Uploads []domain.UploadMeta
	Videos  []domain.MediaRef
	// Block, when set, is waited on before answering (tests of in-flight state).
	Block chan struct{}
}

var _ app.Publisher = (*Publisher)(nil)

// Publish records the upload and returns a fake video id.
func (p *Publisher) Publish(ctx context.Context, video domain.MediaRef, meta domain.UploadMeta) (domain.Publication, error) {
	if p.Block != nil {
		select {
		case <-p.Block:
		case <-ctx.Done():
			return domain.Publication{}, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return domain.Publication{}, p.Err
	}
	p.Uploads = append(p.Uploads, meta)
	p.Videos = append(p.Videos, video)
	id := fmt.Sprintf("yt%03d", len(p.Uploads))
	return domain.Publication{ID: id, URL: "https://www.youtube.com/shorts/" + id}, nil
}

// Count returns how many uploads succeeded.
func (p *Publisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Uploads)
}

// SendAlert records the alert.
func (n *Notifier) SendAlert(_ context.Context, c *domain.Clip, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Alerts = append(n.Alerts, string(c.ID)+": "+text)
	return nil
}

// Snapshot returns copies of the recorded calls.
func (n *Notifier) Snapshot() (sent []domain.ClipID, updates []domain.ClipState, failures []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]domain.ClipID(nil), n.Sent...), append([]domain.ClipState(nil), n.Updates...), append([]string(nil), n.Failures...)
}
