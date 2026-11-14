package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// RecoverPending resumes the work interrupted by a restart: previews of
// clips still in `detected` are sent again and clips in approved, editing or
// rendered go back to the production queue.
type RecoverPending struct {
	repo    ClipRepository
	preview func(ctx context.Context, id domain.ClipID) error
	submit  func(ctx context.Context, id domain.ClipID) bool
	log     *slog.Logger
}

// NewRecoverPending builds the use case.
func NewRecoverPending(repo ClipRepository, preview func(ctx context.Context, id domain.ClipID) error,
	submit func(ctx context.Context, id domain.ClipID) bool, log *slog.Logger) *RecoverPending {
	return &RecoverPending{repo: repo, preview: preview, submit: submit, log: log}
}

// Execute runs the recovery once; it returns how many clips were resumed.
func (r *RecoverPending) Execute(ctx context.Context) (int, error) {
	resumed := 0
	var errs []error
	detected, err := r.repo.ListByState(ctx, domain.StateDetected)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	for _, c := range detected {
		if err := r.preview(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("resend preview of %s: %w", c.ID, err))
			continue
		}
		resumed++
	}
	inProgress, err := r.repo.ListByState(ctx, domain.StateApproved, domain.StateEditing, domain.StateRendered)
	if err != nil {
		return resumed, fmt.Errorf("recover: %w", err)
	}
	for _, c := range inProgress {
		if r.submit(ctx, c.ID) {
			resumed++
		}
	}
	if resumed > 0 {
		r.log.Info("pending work resumed", slog.Int("clips", resumed))
	}
	return resumed, errors.Join(errs...)
}
