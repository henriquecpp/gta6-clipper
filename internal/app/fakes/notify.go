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
	Previews   []domain.PreviewSpec
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

// Notifier records what would be sent to Telegram.
type Notifier struct {
	mu        sync.Mutex
	SendErr   error
	UpdateErr error
	Sent      []domain.ClipID
	Updates   []domain.ClipState
	Failures  []string
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

// Snapshot returns copies of the recorded calls.
func (n *Notifier) Snapshot() (sent []domain.ClipID, updates []domain.ClipState, failures []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]domain.ClipID(nil), n.Sent...), append([]domain.ClipState(nil), n.Updates...), append([]string(nil), n.Failures...)
}
