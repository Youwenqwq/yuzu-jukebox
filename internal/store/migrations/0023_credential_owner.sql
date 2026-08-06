-- +goose Up
-- 凭据所有者绑定：provider 凭据（如 NCM MUSIC_U）是信任委托——
-- 写入凭据的人（扫码登录/管理端设置）即凭据所有者，对账号的写操作
-- （scrobble/like/歌单）仅 owner 可触发。
--
-- credentials 表逐行 INSERT（最新行生效），owner/account 列由
-- UpsertCredential 从上一行继承：任何无身份的写入路径都不丢绑定，
-- 重绑只发生在 API 层（人设凭据 = 重新委托）。
--
-- account_* 是凭据对应平台账号的资料快照（provider 校验凭据时写），
-- 与 owner_principal_id（yuzu 侧委托者）是两回事：cookie 轮换时
-- 账号不变而 owner 换人，或同一 yuzu 用户换绑另一个 NCM 账号。
ALTER TABLE credentials ADD COLUMN owner_principal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE credentials ADD COLUMN account_uid TEXT NOT NULL DEFAULT '';
ALTER TABLE credentials ADD COLUMN account_name TEXT NOT NULL DEFAULT '';
ALTER TABLE credentials ADD COLUMN account_avatar TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE credentials DROP COLUMN account_avatar;
ALTER TABLE credentials DROP COLUMN account_name;
ALTER TABLE credentials DROP COLUMN account_uid;
ALTER TABLE credentials DROP COLUMN owner_principal_id;
