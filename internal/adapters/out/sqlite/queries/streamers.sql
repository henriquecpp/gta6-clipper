-- name: UpsertStreamer :exec
INSERT INTO streamers (login, broadcaster_id, language, allows_clips, layout, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (login) DO UPDATE SET
    broadcaster_id = CASE WHEN excluded.broadcaster_id <> '' THEN excluded.broadcaster_id ELSE streamers.broadcaster_id END,
    language       = excluded.language,
    allows_clips   = excluded.allows_clips,
    layout         = excluded.layout,
    updated_at     = excluded.updated_at;

-- name: ListStreamers :many
SELECT login, broadcaster_id, language, allows_clips, layout, updated_at
FROM streamers
ORDER BY login;
