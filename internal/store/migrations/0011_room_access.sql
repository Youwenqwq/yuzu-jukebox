-- +goose Up
ALTER TABLE rooms ADD COLUMN guest_access_mode TEXT NOT NULL DEFAULT 'open'
    CHECK (guest_access_mode IN ('open', 'static_password', 'rotating_code'));
ALTER TABLE rooms ADD COLUMN guest_code_period_seconds INTEGER NOT NULL DEFAULT 86400
    CHECK (guest_code_period_seconds BETWEEN 60 AND 2592000);

UPDATE rooms
SET guest_access_mode = 'static_password'
WHERE guest_password_hash <> '';
