-- +goose Up
-- 会话持久化：重启后登录态不失效（OIDC 用户免于重扫）。
CREATE TABLE sessions (
    token         TEXT PRIMARY KEY,
    identity_json TEXT NOT NULL,
    expires_at    INTEGER NOT NULL
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
