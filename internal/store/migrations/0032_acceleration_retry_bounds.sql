-- +goose Up
ALTER TABLE distribution_requests ADD COLUMN consecutive_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distribution_requests ADD COLUMN terminal_state TEXT NOT NULL DEFAULT '' CHECK (terminal_state IN ('', 'failed', 'skipped'));
ALTER TABLE distribution_requests ADD COLUMN error_code TEXT NOT NULL DEFAULT '';

-- Count only attempts after the last success when audit history exists. Legacy
-- requests without a success retain their total, so upgrades cannot reset loops.
UPDATE distribution_requests SET consecutive_attempts = CASE
    WHEN EXISTS (SELECT 1 FROM distribution_attempts a WHERE a.acceleration_id = distribution_requests.acceleration_id AND a.track_ref = distribution_requests.track_ref AND a.status = 'succeeded')
    THEN (SELECT COUNT(*) FROM distribution_attempts a WHERE a.acceleration_id = distribution_requests.acceleration_id AND a.track_ref = distribution_requests.track_ref AND a.started_at > (SELECT MAX(s.finished_at) FROM distribution_attempts s WHERE s.acceleration_id = a.acceleration_id AND s.track_ref = a.track_ref AND s.status = 'succeeded'))
    ELSE attempts END
WHERE NOT EXISTS (SELECT 1 FROM distribution_candidates c WHERE c.acceleration_id = distribution_requests.acceleration_id AND c.track_ref = distribution_requests.track_ref);
UPDATE distribution_requests SET terminal_state = 'failed', error_code = 'retry_exhausted', next_attempt_at = 0
WHERE consecutive_attempts >= 5 AND canceled_at = 0 AND evicted_at = 0
AND NOT EXISTS (SELECT 1 FROM distribution_leases l WHERE l.acceleration_id = distribution_requests.acceleration_id AND l.track_ref = distribution_requests.track_ref);
-- Existing long-delay retry jobs get a bounded deadline without interpreting errors.
UPDATE distribution_requests SET next_attempt_at = MIN(next_attempt_at, unixepoch('now') * 1000 + 3600000)
WHERE terminal_state = '' AND next_attempt_at > 0;

ALTER TABLE acceleration_inventory_scans ADD COLUMN next_attempt_at INTEGER NOT NULL DEFAULT 0;
UPDATE acceleration_inventory_scans SET state = 'failed', last_error = 'inventory retry limit reached'
WHERE state = 'queued' AND attempts >= 5;

-- +goose Down
ALTER TABLE acceleration_inventory_scans DROP COLUMN next_attempt_at;
ALTER TABLE distribution_requests DROP COLUMN error_code;
ALTER TABLE distribution_requests DROP COLUMN terminal_state;
ALTER TABLE distribution_requests DROP COLUMN consecutive_attempts;
