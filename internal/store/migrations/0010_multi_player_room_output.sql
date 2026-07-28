-- +goose Up
CREATE TABLE room_player_bindings_v2 (
    player_id  TEXT PRIMARY KEY,
    room_id    TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
);
INSERT INTO room_player_bindings_v2 (player_id, room_id, created_at, updated_at)
SELECT player_id, room_id, created_at, updated_at FROM room_player_bindings;
DROP TABLE room_player_bindings;
ALTER TABLE room_player_bindings_v2 RENAME TO room_player_bindings;
CREATE INDEX idx_room_player_bindings_room
    ON room_player_bindings(room_id, player_id);

-- Absence of a row means the Room has no desired output volume yet. This avoids
-- overriding existing device-local volume during upgrade or first connection.
CREATE TABLE room_output_state (
    room_id    TEXT PRIMARY KEY,
    volume     INTEGER NOT NULL CHECK (volume BETWEEN 0 AND 100),
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE room_output_state;

CREATE TABLE room_player_bindings_v1 (
    room_id    TEXT PRIMARY KEY,
    player_id  TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
);
INSERT INTO room_player_bindings_v1 (room_id, player_id, created_at, updated_at)
SELECT room_id, MIN(player_id), MIN(created_at), MAX(updated_at)
FROM room_player_bindings
GROUP BY room_id;
DROP TABLE room_player_bindings;
ALTER TABLE room_player_bindings_v1 RENAME TO room_player_bindings;
