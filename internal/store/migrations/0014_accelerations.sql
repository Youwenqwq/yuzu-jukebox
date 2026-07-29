-- +goose Up
CREATE TABLE accelerations (
    id                           TEXT PRIMARY KEY,
    name                         TEXT NOT NULL,
    kind                         TEXT NOT NULL CHECK (kind = 'edgeone'),
    enabled                      INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    publish_on_cache_ready       INTEGER NOT NULL DEFAULT 1 CHECK (publish_on_cache_ready IN (0, 1)),
    control_base_url             TEXT NOT NULL DEFAULT '',
    signer_base_url              TEXT NOT NULL DEFAULT '',
    publisher_token_hash         BLOB CHECK (publisher_token_hash IS NULL OR length(publisher_token_hash) = 32),
    publisher_pending_token_hash BLOB CHECK (publisher_pending_token_hash IS NULL OR length(publisher_pending_token_hash) = 32),
    edge_token_hash              BLOB CHECK (edge_token_hash IS NULL OR length(edge_token_hash) = 32),
    edge_pending_token_hash      BLOB CHECK (edge_pending_token_hash IS NULL OR length(edge_pending_token_hash) = 32),
    signer_token                 TEXT NOT NULL DEFAULT '',
    signer_pending_token         TEXT NOT NULL DEFAULT '',
    lease_ttl_seconds            INTEGER NOT NULL DEFAULT 600 CHECK (lease_ttl_seconds > 0),
    upload_rate_bytes_per_second INTEGER NOT NULL DEFAULT 187500 CHECK (upload_rate_bytes_per_second >= 0),
    max_object_bytes             INTEGER NOT NULL DEFAULT 24117248 CHECK (max_object_bytes > 0),
    control_healthy              INTEGER CHECK (control_healthy IS NULL OR control_healthy IN (0, 1)),
    signer_healthy               INTEGER CHECK (signer_healthy IS NULL OR signer_healthy IN (0, 1)),
    health_error                 TEXT NOT NULL DEFAULT '',
    last_health_at               INTEGER,
    created_at                   INTEGER NOT NULL,
    updated_at                   INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_accelerations_publisher_token
    ON accelerations(publisher_token_hash) WHERE publisher_token_hash IS NOT NULL;
CREATE UNIQUE INDEX idx_accelerations_publisher_pending_token
    ON accelerations(publisher_pending_token_hash) WHERE publisher_pending_token_hash IS NOT NULL;
CREATE UNIQUE INDEX idx_accelerations_edge_token
    ON accelerations(edge_token_hash) WHERE edge_token_hash IS NOT NULL;
CREATE UNIQUE INDEX idx_accelerations_edge_pending_token
    ON accelerations(edge_pending_token_hash) WHERE edge_pending_token_hash IS NOT NULL;
CREATE INDEX idx_accelerations_enabled ON accelerations(enabled, id);

-- Preserve P1 rows as disabled resources. An administrator must configure
-- credentials and endpoints before enabling them.
INSERT INTO accelerations (id, name, kind, enabled, created_at, updated_at)
SELECT backend, backend, 'edgeone', 0, MIN(requested_at), MAX(updated_at)
FROM distribution_requests
GROUP BY backend;

CREATE TABLE distribution_requests_new (
    acceleration_id TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    requested_at    INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (acceleration_id, track_ref),
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
INSERT INTO distribution_requests_new
SELECT backend, track_ref, requested_at, updated_at, next_attempt_at, attempts, last_error
FROM distribution_requests;

CREATE TABLE distribution_leases_new (
    id              TEXT PRIMARY KEY,
    acceleration_id TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    owner           TEXT NOT NULL,
    expires_at      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE (acceleration_id, track_ref),
    FOREIGN KEY (acceleration_id, track_ref)
        REFERENCES distribution_requests_new (acceleration_id, track_ref)
        ON DELETE CASCADE
);
INSERT INTO distribution_leases_new
SELECT id, backend, track_ref, owner, expires_at, created_at
FROM distribution_leases;
CREATE INDEX idx_distribution_leases_new_expiry
    ON distribution_leases_new(expires_at);

CREATE TABLE distribution_candidates_new (
    acceleration_id TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    content_version TEXT NOT NULL,
    locator         TEXT NOT NULL,
    layout          TEXT NOT NULL DEFAULT 'object',
    size_bytes      INTEGER NOT NULL,
    content_type    TEXT NOT NULL,
    etag            TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (acceleration_id, track_ref),
    FOREIGN KEY (acceleration_id, track_ref)
        REFERENCES distribution_requests_new (acceleration_id, track_ref)
        ON DELETE CASCADE
);
INSERT INTO distribution_candidates_new
SELECT backend, track_ref, content_version, locator, layout, size_bytes,
       content_type, etag, created_at, updated_at
FROM distribution_candidates;
CREATE INDEX idx_distribution_candidates_new_version
    ON distribution_candidates_new(acceleration_id, content_version);

CREATE TABLE distribution_metrics_new (
    acceleration_id TEXT NOT NULL,
    name            TEXT NOT NULL,
    value           INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (acceleration_id, name),
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
INSERT INTO distribution_metrics_new
SELECT backend, name, value, updated_at FROM distribution_metrics;

DROP TABLE distribution_leases;
DROP TABLE distribution_candidates;
DROP TABLE distribution_requests;
DROP TABLE distribution_metrics;
ALTER TABLE distribution_requests_new RENAME TO distribution_requests;
ALTER TABLE distribution_leases_new RENAME TO distribution_leases;
ALTER TABLE distribution_candidates_new RENAME TO distribution_candidates;
ALTER TABLE distribution_metrics_new RENAME TO distribution_metrics;

CREATE INDEX idx_distribution_leases_expiry ON distribution_leases(expires_at);
CREATE INDEX idx_distribution_candidates_version
    ON distribution_candidates(acceleration_id, content_version);

CREATE TABLE distribution_publishers (
    acceleration_id TEXT NOT NULL,
    owner           TEXT NOT NULL,
    version         TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'idle',
    lease_id        TEXT NOT NULL DEFAULT '',
    track_ref       TEXT NOT NULL DEFAULT '',
    capabilities    TEXT NOT NULL DEFAULT '[]',
    backend_healthy INTEGER NOT NULL DEFAULT 0 CHECK (backend_healthy IN (0, 1)),
    last_error      TEXT NOT NULL DEFAULT '',
    last_seen_at    INTEGER NOT NULL,
    PRIMARY KEY (acceleration_id, owner),
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE INDEX idx_distribution_publishers_seen
    ON distribution_publishers(acceleration_id, last_seen_at);

CREATE TABLE distribution_attempts (
    lease_id        TEXT PRIMARY KEY,
    acceleration_id TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    owner           TEXT NOT NULL,
    phase           TEXT NOT NULL DEFAULT 'claimed',
    source_bytes    INTEGER NOT NULL DEFAULT 0,
    upload_bytes    INTEGER NOT NULL DEFAULT 0,
    total_bytes     INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'active',
    last_error      TEXT NOT NULL DEFAULT '',
    started_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE INDEX idx_distribution_attempts_acceleration_updated
    ON distribution_attempts(acceleration_id, updated_at DESC);
CREATE INDEX idx_distribution_attempts_track
    ON distribution_attempts(acceleration_id, track_ref, started_at DESC);

CREATE TABLE distribution_metric_buckets (
    acceleration_id TEXT NOT NULL,
    bucket_start    INTEGER NOT NULL,
    name            TEXT NOT NULL,
    value           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (acceleration_id, bucket_start, name),
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE INDEX idx_distribution_metric_buckets_time
    ON distribution_metric_buckets(acceleration_id, bucket_start);

-- +goose Down
DROP TABLE distribution_metric_buckets;
DROP TABLE distribution_attempts;
DROP TABLE distribution_publishers;

CREATE TABLE distribution_requests_old (
    backend         TEXT NOT NULL,
    track_ref       TEXT NOT NULL,
    requested_at    INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (backend, track_ref)
);
INSERT INTO distribution_requests_old
SELECT acceleration_id, track_ref, requested_at, updated_at, next_attempt_at, attempts, last_error
FROM distribution_requests;

CREATE TABLE distribution_leases_old (
    id         TEXT PRIMARY KEY,
    backend    TEXT NOT NULL,
    track_ref  TEXT NOT NULL,
    owner      TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (backend, track_ref),
    FOREIGN KEY (backend, track_ref)
        REFERENCES distribution_requests_old (backend, track_ref)
        ON DELETE CASCADE
);
INSERT INTO distribution_leases_old
SELECT id, acceleration_id, track_ref, owner, expires_at, created_at
FROM distribution_leases;

CREATE TABLE distribution_candidates_old (
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
        REFERENCES distribution_requests_old (backend, track_ref)
        ON DELETE CASCADE
);
INSERT INTO distribution_candidates_old
SELECT acceleration_id, track_ref, content_version, locator, layout, size_bytes,
       content_type, etag, created_at, updated_at
FROM distribution_candidates;

CREATE TABLE distribution_metrics_old (
    backend    TEXT NOT NULL,
    name       TEXT NOT NULL,
    value      INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (backend, name)
);
INSERT INTO distribution_metrics_old
SELECT acceleration_id, name, value, updated_at FROM distribution_metrics;

DROP TABLE distribution_leases;
DROP TABLE distribution_candidates;
DROP TABLE distribution_requests;
DROP TABLE distribution_metrics;
ALTER TABLE distribution_requests_old RENAME TO distribution_requests;
ALTER TABLE distribution_leases_old RENAME TO distribution_leases;
ALTER TABLE distribution_candidates_old RENAME TO distribution_candidates;
ALTER TABLE distribution_metrics_old RENAME TO distribution_metrics;
CREATE INDEX idx_distribution_leases_expiry ON distribution_leases(expires_at);
CREATE INDEX idx_distribution_candidates_version
    ON distribution_candidates(backend, content_version);
DROP TABLE accelerations;
