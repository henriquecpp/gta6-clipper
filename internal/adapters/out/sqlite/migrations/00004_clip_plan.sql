-- +goose Up
-- JSON of the validated edit plan (cut, headline, captions, post text), kept
-- so a manual retry does not call the editor (and pay for it) again.
ALTER TABLE clips ADD COLUMN edit_plan TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE clips DROP COLUMN edit_plan;
