-- +goose Up
ALTER TABLE playlists ADD COLUMN bound_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE playlists ADD COLUMN bound_remote_id TEXT NOT NULL DEFAULT '';
ALTER TABLE playlists ADD COLUMN last_sync_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlists ADD COLUMN last_sync_error TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_playlists_bound ON playlists(bound_provider, bound_remote_id) WHERE bound_provider != '';

-- +goose Down
DROP INDEX idx_playlists_bound;
ALTER TABLE playlists DROP COLUMN last_sync_error;
ALTER TABLE playlists DROP COLUMN last_sync_at;
ALTER TABLE playlists DROP COLUMN bound_remote_id;
ALTER TABLE playlists DROP COLUMN bound_provider;
