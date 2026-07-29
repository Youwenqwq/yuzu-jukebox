-- +goose Up
ALTER TABLE accelerations RENAME COLUMN signer_base_url TO backend_base_url;
ALTER TABLE accelerations RENAME COLUMN edge_token_hash TO delivery_token_hash;
ALTER TABLE accelerations RENAME COLUMN edge_pending_token_hash TO delivery_pending_token_hash;
ALTER TABLE accelerations RENAME COLUMN signer_token TO backend_token;
ALTER TABLE accelerations RENAME COLUMN signer_pending_token TO backend_pending_token;
ALTER TABLE accelerations RENAME COLUMN signer_healthy TO backend_healthy;
ALTER TABLE accelerations ADD COLUMN storage_budget_bytes INTEGER NOT NULL DEFAULT 891289600
    CHECK (storage_budget_bytes > 0);
ALTER TABLE accelerations ADD COLUMN storage_high_watermark_percent INTEGER NOT NULL DEFAULT 95
    CHECK (storage_high_watermark_percent BETWEEN 1 AND 100);
ALTER TABLE accelerations ADD COLUMN storage_low_watermark_percent INTEGER NOT NULL DEFAULT 85
    CHECK (storage_low_watermark_percent BETWEEN 1 AND 99);

CREATE TABLE acceleration_objects (
    acceleration_id TEXT NOT NULL,
    locator TEXT NOT NULL,
    content_version TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    external_version TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'ready'
        CHECK (state IN ('ready', 'orphan', 'missing', 'deleting', 'delete_failed')),
    reference_count INTEGER NOT NULL DEFAULT 0 CHECK (reference_count >= 0),
    last_accessed_at INTEGER NOT NULL,
    last_observed_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (acceleration_id, locator),
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE INDEX idx_acceleration_objects_gc
    ON acceleration_objects(acceleration_id, state, last_accessed_at, locator);

INSERT INTO acceleration_objects (
    acceleration_id, locator, content_version, size_bytes, external_version,
    state, reference_count, last_accessed_at, last_observed_at, created_at, updated_at
)
SELECT acceleration_id, locator, MAX(content_version), MAX(size_bytes), MAX(etag),
       'ready', COUNT(*), MAX(updated_at), 0, MIN(created_at), MAX(updated_at)
FROM distribution_candidates
GROUP BY acceleration_id, locator;

CREATE TABLE acceleration_storage_reservations (
    lease_id TEXT PRIMARY KEY,
    acceleration_id TEXT NOT NULL,
    locator TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0),
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (acceleration_id, locator),
    FOREIGN KEY (lease_id) REFERENCES distribution_leases(id) ON DELETE CASCADE,
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE INDEX idx_acceleration_storage_reservations_expiry
    ON acceleration_storage_reservations(acceleration_id, expires_at);

CREATE TABLE acceleration_deletion_jobs (
    id TEXT PRIMARY KEY,
    acceleration_id TEXT NOT NULL,
    locator TEXT NOT NULL,
    owner TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'leased', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    lease_expires_at INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (acceleration_id, locator),
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE INDEX idx_acceleration_deletion_jobs_claim
    ON acceleration_deletion_jobs(acceleration_id, state, lease_expires_at, updated_at);

CREATE TABLE acceleration_inventory_snapshots (
    id TEXT PRIMARY KEY,
    acceleration_id TEXT NOT NULL,
    owner TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE TABLE acceleration_inventory_objects (
    snapshot_id TEXT NOT NULL,
    acceleration_id TEXT NOT NULL,
    locator TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    external_version TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (snapshot_id, locator),
    FOREIGN KEY (snapshot_id) REFERENCES acceleration_inventory_snapshots(id) ON DELETE CASCADE,
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);

CREATE TABLE acceleration_storage_status (
    acceleration_id TEXT PRIMARY KEY,
    observed_bytes INTEGER NOT NULL DEFAULT 0,
    observed_object_count INTEGER NOT NULL DEFAULT 0,
    orphan_count INTEGER NOT NULL DEFAULT 0,
    missing_count INTEGER NOT NULL DEFAULT 0,
    last_reconciled_at INTEGER NOT NULL DEFAULT 0,
    reconciliation_error TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE acceleration_storage_status;
DROP TABLE acceleration_inventory_objects;
DROP TABLE acceleration_inventory_snapshots;
DROP TABLE acceleration_deletion_jobs;
DROP TABLE acceleration_storage_reservations;
DROP TABLE acceleration_objects;

ALTER TABLE accelerations DROP COLUMN storage_low_watermark_percent;
ALTER TABLE accelerations DROP COLUMN storage_high_watermark_percent;
ALTER TABLE accelerations DROP COLUMN storage_budget_bytes;
ALTER TABLE accelerations RENAME COLUMN backend_healthy TO signer_healthy;
ALTER TABLE accelerations RENAME COLUMN backend_pending_token TO signer_pending_token;
ALTER TABLE accelerations RENAME COLUMN backend_token TO signer_token;
ALTER TABLE accelerations RENAME COLUMN delivery_pending_token_hash TO edge_pending_token_hash;
ALTER TABLE accelerations RENAME COLUMN delivery_token_hash TO edge_token_hash;
ALTER TABLE accelerations RENAME COLUMN backend_base_url TO signer_base_url;
