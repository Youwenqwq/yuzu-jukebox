-- +goose Up
ALTER TABLE users ADD COLUMN avatar TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN avatar;
