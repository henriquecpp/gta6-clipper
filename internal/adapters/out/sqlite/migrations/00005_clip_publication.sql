-- +goose Up
-- Telegram message(s) of the final video (edited as the publication
-- progresses) and where the video was published (YouTube Shorts).
ALTER TABLE clips ADD COLUMN final_message TEXT NOT NULL DEFAULT '';
ALTER TABLE clips ADD COLUMN published_id TEXT NOT NULL DEFAULT '';
ALTER TABLE clips ADD COLUMN published_url TEXT NOT NULL DEFAULT '';
ALTER TABLE clips ADD COLUMN publish_error TEXT NOT NULL DEFAULT '';
ALTER TABLE clips ADD COLUMN publishing_at TEXT;
ALTER TABLE clips ADD COLUMN published_at TEXT;

-- +goose Down
ALTER TABLE clips DROP COLUMN published_at;
ALTER TABLE clips DROP COLUMN publishing_at;
ALTER TABLE clips DROP COLUMN publish_error;
ALTER TABLE clips DROP COLUMN published_url;
ALTER TABLE clips DROP COLUMN published_id;
ALTER TABLE clips DROP COLUMN final_message;
