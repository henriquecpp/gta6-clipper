package fakes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/contracttest"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func TestClipRepoContract(t *testing.T) {
	contracttest.RunClipRepository(t, func(*testing.T) app.ClipRepository { return fakes.NewClipRepo() })
}

func TestClipRepoSaveHookAndCancel(t *testing.T) {
	repo := fakes.NewClipRepo()
	boom := errors.New("disk full")
	repo.SaveHook = func(*domain.Clip) error { return boom }
	c := contracttest.FullClip("x", contracttest.FullClip("y", testTime).CreatedAt)
	if err := repo.Save(context.Background(), c); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if repo.Len() != 0 {
		t.Fatal("hooked save stored the clip")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Get(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v", err)
	}
	if _, err := repo.ListByState(ctx, domain.StateDetected); !errors.Is(err, context.Canceled) {
		t.Fatalf("List err = %v", err)
	}
	if err := repo.InTx(ctx, func(context.Context, app.ClipRepository) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("InTx err = %v", err)
	}
}
