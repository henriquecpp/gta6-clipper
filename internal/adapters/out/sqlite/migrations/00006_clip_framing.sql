-- +goose Up
-- Framing decided when the preview was made (auto layout: facecam found in
-- the clip), reused by the final video. stack_regions is JSON.
ALTER TABLE clips ADD COLUMN layout TEXT NOT NULL DEFAULT '';
ALTER TABLE clips ADD COLUMN stack_regions TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE clips DROP COLUMN stack_regions;
ALTER TABLE clips DROP COLUMN layout;
