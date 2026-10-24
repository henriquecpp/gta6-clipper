-- Timestamps are TEXT in ISO-8601 UTC with millisecond precision
-- (2006-01-02T15:04:05.000Z) and ids are TEXT, to stay portable to Postgres.

-- +goose Up
CREATE TABLE streamers (
    login          TEXT    NOT NULL PRIMARY KEY,
    broadcaster_id TEXT    NOT NULL DEFAULT '',
    language       TEXT    NOT NULL,
    allows_clips   INTEGER NOT NULL,
    layout         TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);

-- +goose Down
DROP TABLE streamers;
