-- +goose Up
-- 富元数据：队列/歌单条目的曲目层富字段（快照语义，与 title/artist 一致）
ALTER TABLE room_queue ADD COLUMN album TEXT NOT NULL DEFAULT '';
ALTER TABLE room_queue ADD COLUMN cover_url TEXT NOT NULL DEFAULT '';
ALTER TABLE room_queue ADD COLUMN source_url TEXT NOT NULL DEFAULT '';
ALTER TABLE room_queue ADD COLUMN contributors_json TEXT NOT NULL DEFAULT '';

ALTER TABLE playlist_items ADD COLUMN album TEXT NOT NULL DEFAULT '';
ALTER TABLE playlist_items ADD COLUMN cover_url TEXT NOT NULL DEFAULT '';
ALTER TABLE playlist_items ADD COLUMN source_url TEXT NOT NULL DEFAULT '';
ALTER TABLE playlist_items ADD COLUMN contributors_json TEXT NOT NULL DEFAULT '';

-- 物理层质量信息：下载落库时记录
ALTER TABLE media_cache ADD COLUMN bitrate_kbps INTEGER NOT NULL DEFAULT 0;

-- local provider：上传时从文件 tag 提取
ALTER TABLE media_files ADD COLUMN album TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN cover_path TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN bitrate_kbps INTEGER NOT NULL DEFAULT 0;
