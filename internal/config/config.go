package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type Config struct {
	// 监听地址，如 ":8080"
	Addr string `json:"addr"`
	// SQLite 数据库文件路径
	DBPath string `json:"db_path"`
	// local provider 的媒体文件目录
	MediaDir string `json:"media_dir"`
	// 流式缓存目录
	CacheDir string `json:"cache_dir"`
	// 缓存容量上限（字节），超过后按 LRU 清理
	CacheMaxBytes int64 `json:"cache_max_bytes"`
	// 全局管理员口令：guest 认证时携带即可获得 room_admin/media_admin 角色。
	// v1 没有账号体系，这是唯一的管理员入口。
	AdminPassword string `json:"admin_password"`
	// OIDC 认证（Zitadel 等 IdP）。enabled 且配置完整时
	// 开放 POST /api/v1/auth/oidc。
	OIDC OIDCConfig `json:"oidc"`
	// NCM Provider（NeteaseCloudMusicApi 实例）
	NCM NCMConfig `json:"ncm"`
	// Bili Provider（bilibili-api sidecar 实例）
	Bili BiliConfig `json:"bili"`
}

type OIDCConfig struct {
	Enabled  bool   `json:"enabled"`
	Issuer   string `json:"issuer"`    // IdP issuer，如 https://id.example.org
	ClientID string `json:"client_id"` // Native 应用的 client_id（aud 校验）
	// Zitadel project role → yuzu roles。命中任一 key 即授予对应 value。
	// 未命中者保持 listener/requester 基础角色。
	RoleMapping map[string][]string `json:"role_mapping"`
}

type NCMConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // 如 http://127.0.0.1:3000
	Level   string `json:"level"`    // 音质等级，默认 exhigh
}

type BiliConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // 如 http://127.0.0.1:3002
}

func Default() Config {
	return Config{
		Addr:          ":8080",
		DBPath:        "data/yuzu.db",
		MediaDir:      "data/media",
		CacheDir:      "data/cache",
		CacheMaxBytes: 20 << 30, // 20 GiB
		NCM: NCMConfig{
			Enabled: false,
			BaseURL: "http://127.0.0.1:3000",
			Level:   "exhigh",
		},
		Bili: BiliConfig{
			Enabled: false,
			BaseURL: "http://127.0.0.1:3002",
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// LoadOrCreate 加载配置；文件不存在时以默认值生成一份并返回。
// created 为 true 表示本次是新建。
func LoadOrCreate(path string) (cfg Config, created bool, err error) {
	cfg, err = Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return cfg, false, err
	}
	cfg = Default()
	data, merr := json.MarshalIndent(cfg, "", "  ")
	if merr != nil {
		return cfg, false, merr
	}
	if werr := os.WriteFile(path, append(data, '\n'), 0o644); werr != nil {
		return cfg, false, fmt.Errorf("write default config: %w", werr)
	}
	return cfg, true, nil
}
