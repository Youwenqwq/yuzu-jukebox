-- +goose Up
CREATE TABLE distribution_requests (
    backend         TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    requested_at    INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (backend, track_ref)
);

CREATE TABLE distribution_leases (
    id         TEXT PRIMARY KEY,
    backend    TEXT NOT NULL,
    track_ref  TEXT NOT NULL,
    owner      TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (backend, track_ref),
    FOREIGN KEY (backend, track_ref)
        REFERENCES distribution_requests (backend, track_ref)
        ON DELETE CASCADE
);
CREATE INDEX idx_distribution_leases_expiry
    ON distribution_leases (expires_at);

CREATE TABLE distribution_candidates (
    backend         TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    content_version TEXT NOT NULL,
    locator         TEXT NOT NULL,
    layout          TEXT NOT NULL DEFAULT 'object',
    size_bytes      INTEGER NOT NULL,
    content_type    TEXT NOT NULL,
    etag            TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (backend, track_ref),
    FOREIGN KEY (backend, track_ref)
        REFERENCES distribution_requests (backend, track_ref)
        ON DELETE CASCADE
);
CREATE INDEX idx_distribution_candidates_version
    ON distribution_candidates (backend, content_version);

CREATE TABLE distribution_metrics (
    backend    TEXT NOT NULL,
    name       TEXT NOT NULL,
    value      INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (backend, name)
);

-- +goose Down
DROP TABLE distribution_metrics;
DROP TABLE distribution_candidates;
DROP TABLE distribution_leases;
DROP TABLE distribution_requests;
