-- +goose Up
CREATE TABLE acceleration_inventory_scans (
    id               TEXT PRIMARY KEY,
    acceleration_id  TEXT NOT NULL,
    owner             TEXT NOT NULL DEFAULT '',
    state             TEXT NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'leased', 'completed', 'failed', 'canceled')),
    attempts          INTEGER NOT NULL DEFAULT 0,
    lease_expires_at  INTEGER NOT NULL DEFAULT 0,
    observed_at       INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    requested_at      INTEGER NOT NULL,
    started_at        INTEGER NOT NULL DEFAULT 0,
    completed_at      INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL,
    FOREIGN KEY (acceleration_id) REFERENCES accelerations(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX idx_acceleration_inventory_scans_active
    ON acceleration_inventory_scans(acceleration_id)
    WHERE state IN ('queued', 'leased');
CREATE INDEX idx_acceleration_inventory_scans_claim
    ON acceleration_inventory_scans(acceleration_id, state, lease_expires_at, requested_at);
CREATE INDEX idx_acceleration_inventory_scans_history
    ON acceleration_inventory_scans(acceleration_id, updated_at DESC);

-- +goose Down
DROP TABLE acceleration_inventory_scans;
