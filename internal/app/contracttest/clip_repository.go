// Package contracttest is the reusable contract suite of the ClipRepository
// port. It runs against the in-memory fake and the SQLite adapter, and can run
// unchanged against a future Postgres adapter.
package contracttest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// base is a timestamp with millisecond precision, the precision every
// implementation must preserve.
var base = time.Date(2026, 11, 21, 15, 4, 5, 123_000_000, time.UTC)

// FullClip returns a clip with every field populated.
func FullClip(id domain.ClipID, created time.Time) *domain.Clip {
	c, err := domain.NewClip(domain.NewClipParams{
		ID:       id,
		Streamer: "streamer_a",
		Live:     domain.LiveStream{ID: "stream-1", Game: "Grand Theft Auto VI", Title: "Primeira live — ação!"},
		Moment: domain.Moment{
			Peak:        domain.StreamPos(95 * time.Second),
			Score:       4.25,
			Values:      map[domain.SignalKind]float64{domain.SignalVoice: 3.8, domain.SignalChat: 5.1, domain.SignalClips: 2},
			ViewerClips: 2,
		},
		Window: domain.Window{From: domain.StreamPos(35 * time.Second), To: domain.StreamPos(125 * time.Second)},
		Timeline: []domain.SignalPoint{
			{Pos: domain.StreamPos(35 * time.Second), Voice: 0.5, Chat: 1.25},
			{Pos: domain.StreamPos(36 * time.Second), Voice: 0.75, Chat: 1.5, Clips: 1},
		},
		Source:     "/pending/" + domain.MediaRef(id) + ".ts",
		PeakAt:     created.Add(-30 * time.Second),
		LiveOffset: 83*time.Minute + 25*time.Second,
	}, created)
	if err != nil {
		panic(fmt.Sprintf("contracttest: invalid fixture: %v", err)) // fixture bug, not a runtime condition
	}
	return c
}

func mustSave(t *testing.T, repo app.ClipRepository, c *domain.Clip) {
	t.Helper()
	if err := repo.Save(context.Background(), c); err != nil {
		t.Fatalf("save %s: %v", c.ID, err)
	}
}

func mustGet(t *testing.T, repo app.ClipRepository, id domain.ClipID) *domain.Clip {
	t.Helper()
	c, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return c
}

func assertEqual(t *testing.T, got, want *domain.Clip) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clip mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// RunClipRepository runs the contract against repositories built by newRepo;
// each subtest gets a fresh, empty repository.
func RunClipRepository(t *testing.T, newRepo func(t *testing.T) app.ClipRepository) {
	t.Run("SaveGetRoundTrip", func(t *testing.T) {
		repo := newRepo(t)
		c := FullClip("c-roundtrip", base)
		mustSave(t, repo, c)
		assertEqual(t, mustGet(t, repo, c.ID), c)

		// Every optional field set, through the whole lifecycle.
		for i, s := range []domain.ClipState{domain.StatePreviewSent, domain.StateApproved, domain.StateEditing} {
			if err := c.Transition(s, base.Add(time.Duration(i+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		c.PreviewMedia = "/pending/c-roundtrip.preview.mp4"
		c.PreviewMessage = "123:456,789:10"
		c.FinalMedia = "/output/c-roundtrip.mp4"
		c.Headline = "Ele pulou da ponte!"
		c.PostText = "Momento insano na live de streamer_a"
		c.Usage = domain.Usage{InputTokens: 6000, OutputTokens: 900, CacheWriteTokens: 2100, CacheReadTokens: 50}
		c.Cost = 1312
		if err := c.Fail("render: nvenc unavailable", base.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		mustSave(t, repo, c)
		assertEqual(t, mustGet(t, repo, c.ID), c)
	})

	t.Run("GetNotFound", func(t *testing.T) {
		repo := newRepo(t)
		_, err := repo.Get(context.Background(), "missing")
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("SaveOverwrites", func(t *testing.T) {
		repo := newRepo(t)
		c := FullClip("c-overwrite", base)
		mustSave(t, repo, c)
		if err := c.Transition(domain.StatePreviewSent, base.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		c.PreviewMessage = "1:2"
		mustSave(t, repo, c)
		assertEqual(t, mustGet(t, repo, c.ID), c)
	})

	t.Run("ListByState", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		a := FullClip("c-a", base.Add(2*time.Second))
		b := FullClip("c-b", base)
		d := FullClip("c-d", base.Add(time.Second))
		if err := d.Transition(domain.StatePreviewSent, base.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		e := FullClip("c-e", base.Add(time.Second))
		for _, c := range []*domain.Clip{a, b, d, e} {
			mustSave(t, repo, c)
		}
		got, err := repo.ListByState(ctx, domain.StateDetected)
		if err != nil {
			t.Fatal(err)
		}
		var ids []domain.ClipID
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		if want := []domain.ClipID{"c-b", "c-e", "c-a"}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("ListByState(detected) = %v, want %v (oldest first, then id)", ids, want)
		}
		got, err = repo.ListByState(ctx, domain.StatePreviewSent, domain.StateApproved)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "c-d" {
			t.Fatalf("ListByState(preview_sent, approved) = %v", got)
		}
		assertEqual(t, got[0], d)
		none, err := repo.ListByState(ctx, domain.StateDelivered)
		if err != nil || len(none) != 0 {
			t.Fatalf("ListByState(delivered) = %v, %v", none, err)
		}
		empty, err := repo.ListByState(ctx)
		if err != nil || len(empty) != 0 {
			t.Fatalf("ListByState() = %v, %v", empty, err)
		}
	})

	t.Run("InTxCommit", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		c := FullClip("c-tx", base)
		mustSave(t, repo, c)
		err := repo.InTx(ctx, func(ctx context.Context, tx app.ClipRepository) error {
			cur, err := tx.Get(ctx, c.ID)
			if err != nil {
				return err
			}
			if err := cur.Transition(domain.StatePreviewSent, base.Add(time.Minute)); err != nil {
				return err
			}
			return tx.Save(ctx, cur)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := mustGet(t, repo, c.ID); got.State != domain.StatePreviewSent {
			t.Fatalf("state = %s after commit", got.State)
		}
	})

	t.Run("InTxRollback", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		c := FullClip("c-rollback", base)
		mustSave(t, repo, c)
		boom := errors.New("boom")
		err := repo.InTx(ctx, func(ctx context.Context, tx app.ClipRepository) error {
			cur, err := tx.Get(ctx, c.ID)
			if err != nil {
				return err
			}
			_ = cur.Transition(domain.StatePreviewSent, base.Add(time.Minute)) // valid; rolled back below
			if err := tx.Save(ctx, cur); err != nil {
				return err
			}
			if err := tx.Save(ctx, FullClip("c-new", base)); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		assertEqual(t, mustGet(t, repo, c.ID), c)
		if _, err := repo.Get(ctx, "c-new"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("rolled back insert visible: %v", err)
		}
	})

	t.Run("InTxNested", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		err := repo.InTx(ctx, func(ctx context.Context, tx app.ClipRepository) error {
			return tx.InTx(ctx, func(ctx context.Context, inner app.ClipRepository) error {
				return inner.Save(ctx, FullClip("c-nested", base))
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		mustGet(t, repo, "c-nested")
	})

	t.Run("ConcurrentTransitionsAreIdempotent", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		c := FullClip("c-race", base)
		_ = c.Transition(domain.StatePreviewSent, base)
		mustSave(t, repo, c)

		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- repo.InTx(ctx, func(ctx context.Context, tx app.ClipRepository) error {
					cur, err := tx.Get(ctx, c.ID)
					if err != nil {
						return err
					}
					if err := cur.Transition(domain.StateApproved, base.Add(time.Duration(i)*time.Second)); err != nil {
						return err
					}
					return tx.Save(ctx, cur)
				})
			}()
		}
		wg.Wait()
		close(errs)
		ok, invalid := 0, 0
		for err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrInvalidTransition):
				invalid++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != 1 || invalid != n-1 {
			t.Fatalf("ok=%d invalid=%d, want exactly one approval", ok, invalid)
		}
		if got := mustGet(t, repo, c.ID); got.State != domain.StateApproved || got.Decision != domain.DecisionApproved {
			t.Fatalf("final state %s decision %s", got.State, got.Decision)
		}
	})
}
