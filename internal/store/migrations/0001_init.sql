-- +goose Up
CREATE TABLE rooms (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    guest_password_hash TEXT NOT NULL DEFAULT '',
    policy_json         TEXT NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL
);

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,           -- guest | password | oidc
    name          TEXT NOT NULL,
    password_hash TEXT,
    oidc_subject  TEXT,
    created_at    INTEGER NOT NULL
);

CREATE TABLE credentials (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    provider      TEXT NOT NULL,
    payload       BLOB NOT NULL,           -- 加密后的凭据
    status        TEXT NOT NULL DEFAULT 'unknown', -- unknown | ok | invalid
    last_check_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE media_files (
    id          TEXT PRIMARY KEY,
    filename    TEXT NOT NULL,
    title       TEXT NOT NULL,
    artist      TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL,
    uploaded_by TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE TABLE play_history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    room_id      TEXT NOT NULL,
    track_ref    TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER NOT NULL,
    end_reason   TEXT NOT NULL             -- finished | skipped | error
);
CREATE INDEX idx_play_history_room ON play_history(room_id, started_at);

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id    TEXT NOT NULL,
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL
);

CREATE TABLE media_cache (
    track_ref        TEXT PRIMARY KEY,
    file_path        TEXT NOT NULL,
    size_bytes       INTEGER NOT NULL,
    last_accessed_at INTEGER NOT NULL,
    created_at       INTEGER NOT NULL
);

-- 房间队列持久化：房间重启后恢复队列（播放状态本身不持久化）
CREATE TABLE room_queue (
    room_id      TEXT NOT NULL,
    ord          INTEGER NOT NULL,
    entry_id     TEXT NOT NULL,
    track_ref    TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    artist       TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    requested_by TEXT NOT NULL DEFAULT '',
    added_at     INTEGER NOT NULL,
    PRIMARY KEY (room_id, ord)
);

-- +goose Down
DROP TABLE room_queue;
DROP TABLE media_cache;
DROP TABLE audit_log;
DROP TABLE play_history;
DROP TABLE media_files;
DROP TABLE credentials;
DROP TABLE users;
DROP TABLE rooms;
