-- +goose Up
-- 播放历史补艺人字段：热门/历史卡片需要「标题 + 艺人」双行渲染，
-- 艺人档案端点（/api/v1/artists/{name}）按此聚合。历史行只追加不更新。
ALTER TABLE play_history ADD COLUMN artist TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_play_history_artist ON play_history(artist);

-- +goose Down
DROP INDEX idx_play_history_artist;
ALTER TABLE play_history DROP COLUMN artist;
