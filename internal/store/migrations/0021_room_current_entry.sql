-- +goose Up
-- 正在播放的曲目此前只活在 actor 内存里：它对 SQL 不可见（加速层无法钉住正在
-- 流式传输的对象），重启后也直接丢失（恢复逻辑会跳过它、从再下一首开始）。
-- 改为把当前曲目保留在 room_queue 中，由 is_current 标记游标位置。
--
-- 游标放在 room_queue 而不是 rooms 上：队列整体重写时游标随行一起落库，不存在
-- 「游标指向已不存在的条目」的中间态，也不依赖 rooms 里一定有对应行。
-- 队列的线上表示不变：客户端看到的 queue 仍然只含待播条目。
ALTER TABLE room_queue ADD COLUMN is_current INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE room_queue DROP COLUMN is_current;
