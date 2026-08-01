-- +goose Up
-- 驱逐是缓存的正常工作，不是待重试的失败：GC 回收对象后必须把这个决定写回
-- 需求侧，否则请求会立刻从 ready 翻回 queued，形成"删了又传"的永动循环。
ALTER TABLE distribution_requests ADD COLUMN evicted_at INTEGER NOT NULL DEFAULT 0;
DROP INDEX idx_distribution_requests_claim;
CREATE INDEX idx_distribution_requests_claim
    ON distribution_requests(acceleration_id, evicted_at, canceled_at, cancel_requested_at,
        next_attempt_at, requested_at);

-- +goose Down
DROP INDEX idx_distribution_requests_claim;
CREATE INDEX idx_distribution_requests_claim
    ON distribution_requests(acceleration_id, canceled_at, cancel_requested_at, next_attempt_at, requested_at);
ALTER TABLE distribution_requests DROP COLUMN evicted_at;
