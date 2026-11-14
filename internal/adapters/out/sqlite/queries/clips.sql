-- name: UpsertClip :exec
INSERT INTO clips (
    id, streamer, stream_id, game, stream_title, state,
    peak_ms, window_from_ms, window_to_ms, score, signals,
    source_media, preview_media, final_media, preview_message,
    headline, post_text,
    micro_usd, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
    failure_reason, failed_from, decision, decided_at,
    created_at, updated_at,
    detected_at, preview_sent_at, approved_at, discarded_at, expired_at,
    editing_at, rendered_at, delivered_at, failed_at,
    peak_at, live_offset_ms, edit_plan
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?
)
ON CONFLICT (id) DO UPDATE SET
    streamer           = excluded.streamer,
    stream_id          = excluded.stream_id,
    game               = excluded.game,
    stream_title       = excluded.stream_title,
    state              = excluded.state,
    peak_ms            = excluded.peak_ms,
    window_from_ms     = excluded.window_from_ms,
    window_to_ms       = excluded.window_to_ms,
    score              = excluded.score,
    signals            = excluded.signals,
    source_media       = excluded.source_media,
    preview_media      = excluded.preview_media,
    final_media        = excluded.final_media,
    preview_message    = excluded.preview_message,
    headline           = excluded.headline,
    post_text          = excluded.post_text,
    micro_usd          = excluded.micro_usd,
    input_tokens       = excluded.input_tokens,
    output_tokens      = excluded.output_tokens,
    cache_write_tokens = excluded.cache_write_tokens,
    cache_read_tokens  = excluded.cache_read_tokens,
    failure_reason     = excluded.failure_reason,
    failed_from        = excluded.failed_from,
    decision           = excluded.decision,
    decided_at         = excluded.decided_at,
    created_at         = excluded.created_at,
    updated_at         = excluded.updated_at,
    detected_at        = excluded.detected_at,
    preview_sent_at    = excluded.preview_sent_at,
    approved_at        = excluded.approved_at,
    discarded_at       = excluded.discarded_at,
    expired_at         = excluded.expired_at,
    editing_at         = excluded.editing_at,
    rendered_at        = excluded.rendered_at,
    delivered_at       = excluded.delivered_at,
    failed_at          = excluded.failed_at,
    peak_at            = excluded.peak_at,
    live_offset_ms     = excluded.live_offset_ms,
    edit_plan          = excluded.edit_plan;

-- name: GetClip :one
SELECT * FROM clips WHERE id = ?;

-- name: ListClipsByState :many
SELECT * FROM clips
WHERE state IN (sqlc.slice('states'))
ORDER BY created_at, id;

-- name: UpsertDecision :exec
INSERT INTO decisions (clip_id, streamer, decision, decided_at, score, signals)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (clip_id) DO UPDATE SET
    decision   = excluded.decision,
    decided_at = excluded.decided_at,
    score      = excluded.score,
    signals    = excluded.signals;
