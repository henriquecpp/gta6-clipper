package app

import (
	"context"
	"fmt"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// DiscardClip discards a preview and deletes its media.
func (d *Decisions) DiscardClip(ctx context.Context, id domain.ClipID) error {
	var clip *domain.Clip
	err := d.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
		c, err := repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := c.Transition(domain.StateDiscarded, d.clock.Now()); err != nil {
			return err
		}
		clip = c
		return repo.Save(ctx, c)
	})
	if err != nil {
		return fmt.Errorf("discard clip %s: %w", id, err)
	}
	d.afterFinal(ctx, clip, domain.StateDiscarded)
	return nil
}
