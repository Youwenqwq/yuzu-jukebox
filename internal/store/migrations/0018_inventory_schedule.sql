-- +goose Up
ALTER TABLE accelerations ADD COLUMN inventory_interval_seconds INTEGER NOT NULL DEFAULT 900
    CHECK (inventory_interval_seconds >= 60);
ALTER TABLE accelerations ADD COLUMN inventory_stale_after_seconds INTEGER NOT NULL DEFAULT 1800
    CHECK (inventory_stale_after_seconds >= 60);

-- +goose Down
ALTER TABLE accelerations DROP COLUMN inventory_stale_after_seconds;
ALTER TABLE accelerations DROP COLUMN inventory_interval_seconds;
