-- +goose Up
CREATE TABLE integrations (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    active       INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_used_at INTEGER
);
CREATE INDEX idx_integrations_active_id ON integrations(active, id);

ALTER TABLE sessions ADD COLUMN integration_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_sessions_integration ON sessions(integration_id) WHERE integration_id <> '';

CREATE TABLE idempotency_records (
    actor_id       TEXT NOT NULL,
    integration_id TEXT NOT NULL DEFAULT '',
    key            TEXT NOT NULL,
    method         TEXT NOT NULL,
    path           TEXT NOT NULL,
    request_hash   BLOB NOT NULL CHECK (length(request_hash) = 32),
    status_code    INTEGER,
    response_body  BLOB,
    expires_at     INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (actor_id, integration_id, key, method, path)
);
CREATE INDEX idx_idempotency_expires ON idempotency_records(expires_at);
