package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type CORSConfig struct {
	// 是否启用 CORS。关闭时后端完全不处理跨域头。
	Enabled bool `json:"enabled"`
	// 允许的 Origin。设置 ["*"] 允许任意来源；生产环境应限定具体域名。
	AllowedOrigins []string `json:"allowed_origins"`
	// 是否允许携带凭据（cookies、Authorization 头）。为 true 时 AllowedOrigins 不可为 "*"。
	AllowCredentials bool `json:"allow_credentials"`
}
type MediaConfig struct {
	// 单次媒体上传请求体上限（字节）。
	MaxUploadBytes int64 `json:"max_upload_bytes"`
}

type CacheConfig struct {
	// 单个远端缓存对象的下载上限（字节）；0 表示不限制。
	MaxObjectBytes int64 `json:"max_object_bytes"`
}

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
	// 自动清理超过指定天数未访问的缓存；0 表示关闭
	CacheAutoPruneDays int `json:"cache_auto_prune_days"`
	// 媒体上传配置
	Media MediaConfig `json:"media"`
	// 远端媒体缓存配置
	Cache CacheConfig `json:"cache"`
	// provider 绑定歌单的周期同步间隔（分钟）；0 关闭周期同步，手动同步不受影响
	PlaylistSyncIntervalMinutes int `json:"playlist_sync_interval_minutes"`
	// 全局管理员口令：guest 认证时携带即可获得 room_admin/media_admin 角色。
	// v1 没有账号体系，这是唯一的管理员入口。
	AdminPassword string `json:"admin_password"`
	// 凭据加密主密钥（64 位 hex = 32 字节）。缺省时 LoadOrCreate 自动生成并回写。
	// 用于 credentials 表 AES-GCM 加密；丢失则已存凭据不可解密。
	SecretKey string `json:"secret_key"`
	// OIDC 认证（Zitadel 等 IdP）。enabled 且配置完整时
	// 开放 POST /api/v1/auth/oidc。
	OIDC OIDCConfig `json:"oidc"`
	// CORS 跨域配置
	CORS CORSConfig `json:"cors"`
	// NCM Provider（NeteaseCloudMusicApi 实例）
	NCM NCMConfig `json:"ncm"`
	// Bili Provider（bilibili-api sidecar 实例）
	Bili BiliConfig `json:"bili"`
	// QQ Provider（QQMusicApi sidecar 实例）
	QQ QQConfig `json:"qq"`
}

type OIDCConfig struct {
	Enabled  bool   `json:"enabled"`
	Issuer   string `json:"issuer"`    // IdP issuer，如 https://id.example.org
	ClientID string `json:"client_id"` // Native 应用的 client_id（aud 校验）
	// 额外接受的 client_id（如 WebUI 的 PKCE 应用）。aud 命中 client_id
	// 或任一 extra 即通过；不配时行为与单 client_id 完全一致。
	ExtraClientIDs []string `json:"extra_client_ids,omitempty"`
	// Zitadel project role → yuzu roles。命中任一 key 即授予对应 value。
	// 未命中者保持 listener/requester 基础角色。
	RoleMapping map[string][]string `json:"role_mapping"`
}

type NCMConfig struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"base_url"` // 如 http://127.0.0.1:3000
	Level       string `json:"level"`    // 音质等级，默认 exhigh
	// CoverDirect 封面取图默认模式：true=302 客户端直连（省服务器带宽），
	// false=服务器代理。仅作用于未显式声明 CoverMode 的 provider；
	// ncm/qq 已声明 Redirect，bili 已声明 Proxy（需 Referer，恒代理）。
	CoverDirect bool `json:"cover_direct"`
}

type BiliConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // 如 http://127.0.0.1:3002
}

type QQConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // 如 http://127.0.0.1:8080
	// 音质档位（明文格式，0-16）。12=MP3_320 13=MP3_128 14=ACC_192 7=FLAC；
	// 17+ 为加密格式（需 ekey 解密），播放器无法拉流，拒绝配置。默认 12。
	FileType int `json:"file_type"`
}

func Default() Config {
	return Config{
		Addr:                        ":8080",
		DBPath:                      "data/yuzu.db",
		MediaDir:                    "data/media",
		CacheDir:                    "data/cache",
		CacheMaxBytes:               20 << 30, // 20 GiB
		CacheAutoPruneDays:          0,
		PlaylistSyncIntervalMinutes: 0,
		Media: MediaConfig{
			MaxUploadBytes: 1 << 30,
		},
		Cache: CacheConfig{
			MaxObjectBytes: 512 << 20,
		},
		NCM: NCMConfig{
			Enabled: false,
			BaseURL: "http://127.0.0.1:3000",
			Level:   "exhigh",
		},
		CORS: CORSConfig{
			Enabled:          false,
			AllowedOrigins:   []string{"*"},
			AllowCredentials: false,
		},
		Bili: BiliConfig{
			Enabled: false,
			BaseURL: "http://127.0.0.1:3002",
		},
		QQ: QQConfig{
			Enabled:  false,
			BaseURL:  "http://127.0.0.1:8080",
			FileType: 12,
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
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// LoadOrCreate 加载配置；文件不存在时以默认值生成一份并返回。
// created 为 true 表示本次是新建。
// secret_key 缺失时自动生成并回写文件（凭据落盘加密需要）。
func LoadOrCreate(path string) (cfg Config, created bool, err error) {
	cfg, err = Load(path)
	if err == nil {
		if cfg.SecretKey == "" {
			if werr := ensureSecretKey(path, &cfg); werr != nil {
				return cfg, false, werr
			}
		}
		return cfg, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return cfg, false, err
	}
	cfg = Default()
	cfg.SecretKey = generateSecretKey()
	data, merr := json.MarshalIndent(cfg, "", "  ")
	if merr != nil {
		return cfg, false, merr
	}
	if werr := os.WriteFile(path, append(data, '\n'), 0o600); werr != nil {
		return cfg, false, fmt.Errorf("write default config: %w", werr)
	}
	return cfg, true, nil
}

// ensureSecretKey 为已有配置补生成 secret_key 并回写。
func ensureSecretKey(path string, cfg *Config) error {
	cfg.SecretKey = generateSecretKey()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func generateSecretKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// SecretKeyBytes 解码 hex 形式的 secret_key。
func (c Config) SecretKeyBytes() ([]byte, error) {
	if c.SecretKey == "" {
		return nil, nil
	}
	return hex.DecodeString(c.SecretKey)
}
