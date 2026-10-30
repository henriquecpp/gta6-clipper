package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/sqlite/sqlcgen"
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// tsLayout is the single timestamp format of the database: ISO-8601 UTC with
// millisecond precision.
const tsLayout = "2006-01-02T15:04:05.000Z"

// Repo is the SQLite ClipRepository (Repository pattern).
type Repo struct {
	db   *sql.DB
	q    *sqlcgen.Queries
	inTx bool
}

var _ app.ClipRepository = (*Repo)(nil)

// NewRepo builds a repository over an opened database (see Open).
func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db, q: sqlcgen.New(db)}
}

// InTx runs fn in a transaction. Nested calls reuse the outer transaction.
func (r *Repo) InTx(ctx context.Context, fn func(ctx context.Context, repo app.ClipRepository) error) error {
	if r.inTx {
		return fn(ctx, r)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	txRepo := &Repo{db: r.db, q: r.q.WithTx(tx), inTx: true}
	if err := fn(ctx, txRepo); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Save upserts the clip and, when it carries a human decision, the decision row.
func (r *Repo) Save(ctx context.Context, c *domain.Clip) error {
	return r.InTx(ctx, func(ctx context.Context, repo app.ClipRepository) error {
		return repo.(*Repo).save(ctx, c)
	})
}

func (r *Repo) save(ctx context.Context, c *domain.Clip) error {
	signals, err := encodeSignals(c)
	if err != nil {
		return err
	}
	st := func(s domain.ClipState) sql.NullString { return nullTime(c.StateTimes[s]) }
	p := sqlcgen.UpsertClipParams{
		ID:               string(c.ID),
		Streamer:         c.Streamer,
		StreamID:         string(c.StreamID),
		Game:             c.Game,
		StreamTitle:      c.StreamTitle,
		State:            string(c.State),
		PeakMs:           time.Duration(c.Moment.Peak).Milliseconds(),
		WindowFromMs:     time.Duration(c.Window.From).Milliseconds(),
		WindowToMs:       time.Duration(c.Window.To).Milliseconds(),
		Score:            c.Moment.Score,
		Signals:          signals,
		SourceMedia:      string(c.SourceMedia),
		PreviewMedia:     string(c.PreviewMedia),
		FinalMedia:       string(c.FinalMedia),
		PreviewMessage:   string(c.PreviewMessage),
		Headline:         c.Headline,
		PostText:         c.PostText,
		MicroUsd:         int64(c.Cost),
		InputTokens:      c.Usage.InputTokens,
		OutputTokens:     c.Usage.OutputTokens,
		CacheWriteTokens: c.Usage.CacheWriteTokens,
		CacheReadTokens:  c.Usage.CacheReadTokens,
		FailureReason:    c.FailureReason,
		FailedFrom:       string(c.FailedFrom),
		Decision:         string(c.Decision),
		DecidedAt:        nullTime(c.DecidedAt),
		CreatedAt:        formatTime(c.CreatedAt),
		UpdatedAt:        formatTime(c.UpdatedAt),
		DetectedAt:       st(domain.StateDetected),
		PreviewSentAt:    st(domain.StatePreviewSent),
		ApprovedAt:       st(domain.StateApproved),
		DiscardedAt:      st(domain.StateDiscarded),
		ExpiredAt:        st(domain.StateExpired),
		EditingAt:        st(domain.StateEditing),
		RenderedAt:       st(domain.StateRendered),
		DeliveredAt:      st(domain.StateDelivered),
		FailedAt:         st(domain.StateFailed),
	}
	if err := r.q.UpsertClip(ctx, p); err != nil {
		return fmt.Errorf("upsert clip %s: %w", c.ID, err)
	}
	if c.Decision != "" {
		err := r.q.UpsertDecision(ctx, sqlcgen.UpsertDecisionParams{
			ClipID:    string(c.ID),
			Streamer:  c.Streamer,
			Decision:  string(c.Decision),
			DecidedAt: formatTime(c.DecidedAt),
			Score:     c.Moment.Score,
			Signals:   signals,
		})
		if err != nil {
			return fmt.Errorf("upsert decision for clip %s: %w", c.ID, err)
		}
	}
	return nil
}

// Get loads a clip by id.
func (r *Repo) Get(ctx context.Context, id domain.ClipID) (*domain.Clip, error) {
	row, err := r.q.GetClip(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("clip %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get clip %s: %w", id, err)
	}
	return toDomain(row)
}

// ListByState returns the clips in the given states, oldest first.
func (r *Repo) ListByState(ctx context.Context, states ...domain.ClipState) ([]*domain.Clip, error) {
	out := []*domain.Clip{}
	if len(states) == 0 {
		return out, nil
	}
	ss := make([]string, len(states))
	for i, s := range states {
		ss[i] = string(s)
	}
	rows, err := r.q.ListClipsByState(ctx, ss)
	if err != nil {
		return nil, fmt.Errorf("list clips by state: %w", err)
	}
	for _, row := range rows {
		c, err := toDomain(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// SyncStreamers mirrors the allowlist into the streamers table (reference only).
func (r *Repo) SyncStreamers(ctx context.Context, streamers []domain.Streamer, now time.Time) error {
	for _, s := range streamers {
		allows := int64(0)
		if s.AllowsClips {
			allows = 1
		}
		err := r.q.UpsertStreamer(ctx, sqlcgen.UpsertStreamerParams{
			Login:       s.Login,
			Language:    s.Language,
			AllowsClips: allows,
			Layout:      string(s.Layout),
			UpdatedAt:   formatTime(now),
		})
		if err != nil {
			return fmt.Errorf("sync streamer %s: %w", s.Login, err)
		}
	}
	return nil
}

type signalsJSON struct {
	Values      map[string]float64 `json:"values,omitempty"`
	ViewerClips int                `json:"viewer_clips,omitempty"`
	Late        bool               `json:"late,omitempty"`
	Timeline    []pointJSON        `json:"timeline,omitempty"`
}

type pointJSON struct {
	PosMS int64   `json:"pos_ms"`
	Voice float64 `json:"voice"`
	Chat  float64 `json:"chat"`
	Clips float64 `json:"clips"`
}

func encodeSignals(c *domain.Clip) (string, error) {
	s := signalsJSON{ViewerClips: c.Moment.ViewerClips, Late: c.Moment.Late}
	if len(c.Moment.Values) > 0 {
		s.Values = make(map[string]float64, len(c.Moment.Values))
		for k, v := range c.Moment.Values {
			s.Values[string(k)] = v
		}
	}
	for _, p := range c.Timeline {
		s.Timeline = append(s.Timeline, pointJSON{PosMS: time.Duration(p.Pos).Milliseconds(), Voice: p.Voice, Chat: p.Chat, Clips: p.Clips})
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode signals of clip %s: %w", c.ID, err)
	}
	return string(b), nil
}

func toDomain(row sqlcgen.Clip) (*domain.Clip, error) {
	var sig signalsJSON
	if err := json.Unmarshal([]byte(row.Signals), &sig); err != nil {
		return nil, fmt.Errorf("decode signals of clip %s: %w", row.ID, err)
	}
	created, err := parseTime(row.CreatedAt)
	if err != nil {
		return nil, err
	}
	updated, err := parseTime(row.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c := &domain.Clip{
		ID:          domain.ClipID(row.ID),
		Streamer:    row.Streamer,
		StreamID:    domain.StreamID(row.StreamID),
		Game:        row.Game,
		StreamTitle: row.StreamTitle,
		State:       domain.ClipState(row.State),
		Moment: domain.Moment{
			Peak:        msPos(row.PeakMs),
			Score:       row.Score,
			ViewerClips: sig.ViewerClips,
			Late:        sig.Late,
		},
		Window:         domain.Window{From: msPos(row.WindowFromMs), To: msPos(row.WindowToMs)},
		SourceMedia:    domain.MediaRef(row.SourceMedia),
		PreviewMedia:   domain.MediaRef(row.PreviewMedia),
		FinalMedia:     domain.MediaRef(row.FinalMedia),
		PreviewMessage: domain.MessageRef(row.PreviewMessage),
		Headline:       row.Headline,
		PostText:       row.PostText,
		Usage: domain.Usage{
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			CacheWriteTokens: row.CacheWriteTokens,
			CacheReadTokens:  row.CacheReadTokens,
		},
		Cost:          domain.MicroUSD(row.MicroUsd),
		FailureReason: row.FailureReason,
		FailedFrom:    domain.ClipState(row.FailedFrom),
		Decision:      domain.Decision(row.Decision),
		CreatedAt:     created,
		UpdatedAt:     updated,
		StateTimes:    map[domain.ClipState]time.Time{},
	}
	if !c.State.Valid() {
		return nil, fmt.Errorf("clip %s has unknown state %q", row.ID, row.State)
	}
	if len(sig.Values) > 0 {
		c.Moment.Values = make(map[domain.SignalKind]float64, len(sig.Values))
		for k, v := range sig.Values {
			c.Moment.Values[domain.SignalKind(k)] = v
		}
	}
	for _, p := range sig.Timeline {
		c.Timeline = append(c.Timeline, domain.SignalPoint{Pos: msPos(p.PosMS), Voice: p.Voice, Chat: p.Chat, Clips: p.Clips})
	}
	if c.DecidedAt, err = parseNullTime(row.DecidedAt); err != nil {
		return nil, err
	}
	stateCols := map[domain.ClipState]sql.NullString{
		domain.StateDetected:    row.DetectedAt,
		domain.StatePreviewSent: row.PreviewSentAt,
		domain.StateApproved:    row.ApprovedAt,
		domain.StateDiscarded:   row.DiscardedAt,
		domain.StateExpired:     row.ExpiredAt,
		domain.StateEditing:     row.EditingAt,
		domain.StateRendered:    row.RenderedAt,
		domain.StateDelivered:   row.DeliveredAt,
		domain.StateFailed:      row.FailedAt,
	}
	for s, v := range stateCols {
		t, err := parseNullTime(v)
		if err != nil {
			return nil, err
		}
		if !t.IsZero() {
			c.StateTimes[s] = t
		}
	}
	return c, nil
}

func msPos(ms int64) domain.StreamPos { return domain.StreamPos(time.Duration(ms) * time.Millisecond) }

func formatTime(t time.Time) string { return t.UTC().Format(tsLayout) }

func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func parseNullTime(v sql.NullString) (time.Time, error) {
	if !v.Valid || v.String == "" {
		return time.Time{}, nil
	}
	return parseTime(v.String)
}
