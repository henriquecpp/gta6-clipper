package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/contracttest"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "clipper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRepo(db)
}

func TestRepoContract(t *testing.T) {
	contracttest.RunClipRepository(t, func(t *testing.T) app.ClipRepository { return newTestRepo(t) })
}

func TestDecisionRowsAreRecorded(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	c := contracttest.FullClip("c-decision", now)
	if err := repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	count := func() (n int, decision string) {
		row := repo.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(decision), '') FROM decisions WHERE clip_id = ?", c.ID)
		if err := row.Scan(&n, &decision); err != nil {
			t.Fatal(err)
		}
		return n, decision
	}
	if n, _ := count(); n != 0 {
		t.Fatalf("decision recorded before any decision: %d", n)
	}
	_ = c.Transition(domain.StatePreviewSent, now.Add(time.Second))
	_ = c.Transition(domain.StateDiscarded, now.Add(time.Minute))
	if err := repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n, d := count(); n != 1 || d != string(domain.DecisionDiscarded) {
		t.Fatalf("decisions = %d %q", n, d)
	}
}

func TestSyncStreamers(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	ss := []domain.Streamer{{Login: "a", Language: "en", AllowsClips: true, Layout: domain.LayoutBlur}, {Login: "b", Language: "pt", Layout: domain.LayoutCrop}}
	if err := repo.SyncStreamers(ctx, ss, now); err != nil {
		t.Fatal(err)
	}
	ss[1].Layout = domain.LayoutStack
	if err := repo.SyncStreamers(ctx, ss, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.q.ListStreamers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Layout != "stack" || rows[0].AllowsClips != 1 || rows[1].UpdatedAt != "2026-11-21T11:00:00.000Z" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestCorruptRowIsReported(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	c := contracttest.FullClip("c-bad", time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC))
	if err := repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, "UPDATE clips SET state = 'bogus' WHERE id = ?", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, c.ID); err == nil {
		t.Fatal("expected error for unknown state")
	}
	if _, err := repo.db.ExecContext(ctx, "UPDATE clips SET state = 'detected', signals = '{' WHERE id = ?", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ListByState(ctx, domain.StateDetected); err == nil {
		t.Fatal("expected error for corrupt signals")
	}
}
