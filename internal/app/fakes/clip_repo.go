// Package fakes provides hand-written in-memory implementations of the app
// ports for use-case tests (no mocking frameworks, CLAUDE.md §17).
package fakes

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ClipRepo is an in-memory ClipRepository. Transactions are serialized and
// work on a snapshot that replaces the store only on success, mirroring the
// single-connection SQLite repository.
type ClipRepo struct {
	mu    sync.Mutex
	clips map[domain.ClipID]*domain.Clip
	inTx  bool

	// SaveHook, when set, is called before every Save; a non-nil error aborts it.
	SaveHook func(c *domain.Clip) error
}

var _ app.ClipRepository = (*ClipRepo)(nil)

// NewClipRepo returns an empty repository.
func NewClipRepo() *ClipRepo {
	return &ClipRepo{clips: make(map[domain.ClipID]*domain.Clip)}
}

func (r *ClipRepo) lock() {
	if !r.inTx {
		r.mu.Lock()
	}
}

func (r *ClipRepo) unlock() {
	if !r.inTx {
		r.mu.Unlock()
	}
}

// Save stores a deep copy of c.
func (r *ClipRepo) Save(ctx context.Context, c *domain.Clip) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.SaveHook != nil {
		if err := r.SaveHook(c); err != nil {
			return err
		}
	}
	r.lock()
	defer r.unlock()
	r.clips[c.ID] = c.Clone()
	return nil
}

// Get returns a deep copy of the clip.
func (r *ClipRepo) Get(ctx context.Context, id domain.ClipID) (*domain.Clip, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.lock()
	defer r.unlock()
	c, ok := r.clips[id]
	if !ok {
		return nil, fmt.Errorf("clip %s: %w", id, domain.ErrNotFound)
	}
	return c.Clone(), nil
}

// ListByState returns copies of the clips in the given states, oldest first.
func (r *ClipRepo) ListByState(ctx context.Context, states ...domain.ClipState) ([]*domain.Clip, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.lock()
	defer r.unlock()
	out := []*domain.Clip{}
	for _, c := range r.clips {
		if slices.Contains(states, c.State) {
			out = append(out, c.Clone())
		}
	}
	slices.SortFunc(out, func(a, b *domain.Clip) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// InTx runs fn against a snapshot and commits it if fn succeeds.
func (r *ClipRepo) InTx(ctx context.Context, fn func(ctx context.Context, repo app.ClipRepository) error) error {
	if r.inTx {
		return fn(ctx, r)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx := &ClipRepo{clips: make(map[domain.ClipID]*domain.Clip, len(r.clips)), inTx: true, SaveHook: r.SaveHook}
	for id, c := range r.clips {
		tx.clips[id] = c.Clone()
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	r.clips = tx.clips
	return nil
}

// Len returns how many clips are stored.
func (r *ClipRepo) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.clips)
}
