-- +goose Up
CREATE TABLE players (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   BLOB UNIQUE CHECK (token_hash IS NULL OR length(token_hash) = 32),
    active       INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_seen_at INTEGER,
    CHECK (active = 0 OR token_hash IS NOT NULL)
);

-- Existing bindings came from self-declared player IDs and have no credential.
-- Preserve them as inactive resources; an administrator must rotate a key and
-- explicitly enable them before they can authenticate.
INSERT INTO players (id, name, token_hash, active, created_at, updated_at)
SELECT player_id, player_id, NULL, 0, MIN(created_at), MAX(updated_at)
FROM room_player_bindings
GROUP BY player_id;

CREATE TABLE room_player_bindings_v3 (
    player_id  TEXT PRIMARY KEY,
    room_id    TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (player_id) REFERENCES players(id) ON DELETE CASCADE,
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
);
INSERT INTO room_player_bindings_v3 (player_id, room_id, created_at, updated_at)
SELECT player_id, room_id, created_at, updated_at FROM room_player_bindings;
DROP TABLE room_player_bindings;
ALTER TABLE room_player_bindings_v3 RENAME TO room_player_bindings;
CREATE INDEX idx_room_player_bindings_room
    ON room_player_bindings(room_id, player_id);
