-- +goose Up
-- Global principals keep mutable authorization state separate from session snapshots.
ALTER TABLE users ADD COLUMN roles_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE users ADD COLUMN active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1));
ALTER TABLE users ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE users SET updated_at = created_at WHERE updated_at = 0;

-- A non-empty OIDC subject identifies at most one principal.
CREATE UNIQUE INDEX idx_users_oidc_subject
    ON users(oidc_subject)
    WHERE oidc_subject IS NOT NULL AND oidc_subject <> '';

CREATE TABLE external_identity_links (
    integration_id TEXT NOT NULL,
    adapter_id     TEXT NOT NULL,
    scope_type     TEXT NOT NULL,
    scope_id       TEXT NOT NULL,
    subject_id     TEXT NOT NULL,
    principal_id   TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (integration_id, adapter_id, scope_type, scope_id, subject_id),
    FOREIGN KEY (principal_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX idx_external_identity_links_principal
    ON external_identity_links(principal_id);

CREATE TABLE external_scope_rooms (
    integration_id TEXT NOT NULL,
    adapter_id     TEXT NOT NULL,
    scope_type     TEXT NOT NULL,
    scope_id       TEXT NOT NULL,
    room_id        TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (integration_id, adapter_id, scope_type, scope_id),
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
);
CREATE INDEX idx_external_scope_rooms_room
    ON external_scope_rooms(room_id);

CREATE TABLE room_principal_grants (
    room_id      TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    capability   TEXT NOT NULL,
    granted_at   INTEGER NOT NULL,
    PRIMARY KEY (room_id, principal_id, capability),
    FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX idx_room_principal_grants_principal
    ON room_principal_grants(principal_id, capability, room_id);

