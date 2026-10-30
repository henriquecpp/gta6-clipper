-- +goose Up
CREATE TABLE clips (
    id                 TEXT    NOT NULL PRIMARY KEY,
    streamer           TEXT    NOT NULL,
    stream_id          TEXT    NOT NULL,
    game               TEXT    NOT NULL DEFAULT '',
    stream_title       TEXT    NOT NULL DEFAULT '',
    state              TEXT    NOT NULL,
    peak_ms            INTEGER NOT NULL,
    window_from_ms     INTEGER NOT NULL,
    window_to_ms       INTEGER NOT NULL,
    score              REAL    NOT NULL,
    -- JSON: signal values at the peak, viewer clip count, late flag and the
    -- per-second timeline over the window (small; dense series are not stored).
    signals            TEXT    NOT NULL DEFAULT '{}',
    source_media       TEXT    NOT NULL DEFAULT '',
    preview_media      TEXT    NOT NULL DEFAULT '',
    final_media        TEXT    NOT NULL DEFAULT '',
    preview_message    TEXT    NOT NULL DEFAULT '',
    headline           TEXT    NOT NULL DEFAULT '',
    post_text          TEXT    NOT NULL DEFAULT '',
    micro_usd          INTEGER NOT NULL DEFAULT 0,
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    failure_reason     TEXT    NOT NULL DEFAULT '',
    failed_from        TEXT    NOT NULL DEFAULT '',
    decision           TEXT    NOT NULL DEFAULT '',
    decided_at         TEXT,
    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL,
    detected_at        TEXT,
    preview_sent_at    TEXT,
    approved_at        TEXT,
    discarded_at       TEXT,
    expired_at         TEXT,
    editing_at         TEXT,
    rendered_at        TEXT,
    delivered_at       TEXT,
    failed_at          TEXT
);

CREATE INDEX clips_state_created_idx ON clips (state, created_at, id);

-- Human decisions on previews: the dataset used to calibrate the scorer.
CREATE TABLE decisions (
    clip_id    TEXT NOT NULL PRIMARY KEY REFERENCES clips (id) ON DELETE CASCADE,
    streamer   TEXT NOT NULL,
    decision   TEXT NOT NULL,
    decided_at TEXT NOT NULL,
    score      REAL NOT NULL,
    signals    TEXT NOT NULL
);

-- +goose Down
DROP TABLE decisions;
DROP TABLE clips;
