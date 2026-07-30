-- +goose Up
ALTER TABLE distribution_requests ADD COLUMN cancel_requested_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distribution_requests ADD COLUMN canceled_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_distribution_requests_claim
    ON distribution_requests(acceleration_id, canceled_at, cancel_requested_at, next_attempt_at, requested_at);

-- +goose Down
DROP INDEX idx_distribution_requests_claim;
ALTER TABLE distribution_requests DROP COLUMN canceled_at;
ALTER TABLE distribution_requests DROP COLUMN cancel_requested_at;
