package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ExpireClips expires every preview past its TTL; it returns how many expired.
func (d *Decisions) ExpireClips(ctx context.Context) (int, error) {
	clips, err := d.repo.ListByState(ctx, domain.StatePreviewSent)
	if err != nil {
		return 0, fmt.Errorf("list previews: %w", err)
	}
	n := 0
	var errs []error
	for _, c := range clips {
		if !c.IsExpired(d.clock.Now(), d.ttl) {
			continue
		}
		var clip *domain.Clip
		err := d.repo.InTx(ctx, func(ctx context.Context, repo ClipRepository) error {
			cur, err := repo.Get(ctx, c.ID)
			if err != nil {
				return err
			}
			if !cur.IsExpired(d.clock.Now(), d.ttl) {
				return nil // decided meanwhile
			}
			if err := cur.Transition(domain.StateExpired, d.clock.Now()); err != nil {
				return err
			}
			clip = cur
			return repo.Save(ctx, cur)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("expire clip %s: %w", c.ID, err))
			continue
		}
		if clip != nil {
			d.afterFinal(ctx, clip, domain.StateExpired)
			n++
		}
	}
	return n, errors.Join(errs...)
}

// ExpireTask adapts ExpireClips to the scheduler.
func (d *Decisions) ExpireTask(ctx context.Context) error {
	_, err := d.ExpireClips(ctx)
	return err
}
