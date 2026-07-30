-- +goose Up
ALTER TABLE rooms ADD COLUMN trusted_roles_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE rooms DROP COLUMN trusted_roles_json;
