-- +goose Up
-- 歌单置顶（Library 排序）：pinned=1 排前，其余按创建时间。
ALTER TABLE playlists ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE playlists DROP COLUMN pinned;
