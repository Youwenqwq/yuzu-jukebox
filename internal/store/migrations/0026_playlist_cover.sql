-- +goose Up
ALTER TABLE playlists ADD COLUMN cover_url TEXT NOT NULL DEFAULT '';
ALTER TABLE playlists ADD COLUMN cover_path TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE playlists DROP COLUMN cover_path;
ALTER TABLE playlists DROP COLUMN cover_url;
