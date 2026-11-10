-- +goose Up
ALTER TABLE clips ADD COLUMN peak_at TEXT;
ALTER TABLE clips ADD COLUMN live_offset_ms INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE clips DROP COLUMN live_offset_ms;
ALTER TABLE clips DROP COLUMN peak_at;
