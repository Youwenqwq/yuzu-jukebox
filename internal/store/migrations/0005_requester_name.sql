-- +goose Up
-- 点歌人显示名：入队时快照，后续身份改名不回写历史队列条目
ALTER TABLE room_queue ADD COLUMN requester_name TEXT NOT NULL DEFAULT '';
