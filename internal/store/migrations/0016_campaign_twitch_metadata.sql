-- +goose Up
-- +goose StatementBegin
ALTER TABLE campaigns ADD COLUMN twitch_game_id TEXT NOT NULL DEFAULT '';
ALTER TABLE campaigns ADD COLUMN twitch_game_slug TEXT NOT NULL DEFAULT '';
-- Existing values may be Twitch timestamps or legacy estimates. Do not
-- present them as verified provider times until a fresh discovery confirms them.
ALTER TABLE campaigns ADD COLUMN starts_at_source TEXT NOT NULL DEFAULT 'legacy_unknown';
ALTER TABLE campaigns ADD COLUMN ends_at_source TEXT NOT NULL DEFAULT 'legacy_unknown';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE campaigns DROP COLUMN ends_at_source;
ALTER TABLE campaigns DROP COLUMN starts_at_source;
ALTER TABLE campaigns DROP COLUMN twitch_game_slug;
ALTER TABLE campaigns DROP COLUMN twitch_game_id;
-- +goose StatementEnd
