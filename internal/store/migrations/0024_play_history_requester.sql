-- +goose Up
-- 跨房间按点歌人读取历史若无专用索引会全表扫描；started_at 保持最新优先查询高效。
CREATE INDEX idx_play_history_requester ON play_history(requested_by, started_at);

-- +goose Down
DROP INDEX idx_play_history_requester;
