-- +goose Up
CREATE TABLE playlists (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE playlist_items (
    playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    ord         INTEGER NOT NULL,
    track_ref   TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    artist      TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    added_at    INTEGER NOT NULL,
    PRIMARY KEY (playlist_id, ord)
);
CREATE INDEX idx_playlist_items_ref ON playlist_items(track_ref);

-- +goose Down
DROP TABLE playlist_items;
DROP TABLE playlists;
