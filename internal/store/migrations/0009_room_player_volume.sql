-- +goose Up
ALTER TABLE sessions ADD COLUMN integration_adapter_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN integration_scope_type TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN integration_scope_id TEXT NOT NULL DEFAULT '';

CREATE TABLE room_player_bindings (
    room_id      TEXT PRIMARY KEY,
    player_id    TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
);
