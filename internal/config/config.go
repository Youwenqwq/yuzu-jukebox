package config

import (
	"encoding/json"
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
	// NCM Provider（NeteaseCloudMusicApi 实例）
	NCM NCMConfig `json:"ncm"`
}

type NCMConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // 如 http://127.0.0.1:3000
	Level   string `json:"level"`    // 音质等级，默认 exhigh
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
