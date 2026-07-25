// Package provider 定义媒体来源的统一抽象。
//
// 核心模型是两级结构：
//   - TrackRef（逻辑层，持久化、可入队列）："provider:id" 形式的字符串
//   - StreamLocator（物理层，临时、可过期）：Resolve 兑换出的可拉流地址
//
// 队列里只存 TrackRef；StreamLocator 由缓存层在临近播放时 Resolve，
// 凭据（Cookie 等）只存在于 Provider 内部，永不下发给客户端。
package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TrackRef 是媒体条目的全局逻辑引用，格式 "<provider>:<id>"。
type TrackRef string

func NewRef(providerID, id string) TrackRef { return TrackRef(providerID + ":" + id) }

func (r TrackRef) String() string { return string(r) }

// Split 拆出 provider 与 id。非法格式返回错误。
func (r TrackRef) Split() (providerID, id string, err error) {
	p, rest, ok := strings.Cut(string(r), ":")
	if !ok || p == "" || rest == "" {
		return "", "", fmt.Errorf("invalid track_ref %q, want \"provider:id\"", string(r))
	}
	return p, rest, nil
}

// Track 是媒体元数据。
type Track struct {
	Ref        TrackRef `json:"track_ref"`
	Title      string   `json:"title"`
	Artist     string   `json:"artist"`
	DurationMs int64    `json:"duration_ms"`
}

// StreamLocator 是 Resolve 的结果：拉流所需的全部信息。
type StreamLocator struct {
	// URL 为拉流地址。local provider 使用 "file://" 前缀表示本地文件，
	// 缓存层对此短路：直接服用原文件，不经过网络缓存。
	URL        string
	Header     http.Header // 拉流时需携带的头（Referer/UA/Cookie），仅服务端使用
	Format     string      // 如 "mp3" "flac" "m4a"，可能为空
	DurationMs int64
	ExpiresAt  time.Time // 零值表示不过期
}

func (l StreamLocator) IsFile() bool { return strings.HasPrefix(l.URL, "file://") }

func (l StreamLocator) FilePath() string { return strings.TrimPrefix(l.URL, "file://") }

// Provider 是媒体来源适配器。实现必须并发安全。
type Provider interface {
	// ID 是 provider 标识，即 TrackRef 的前缀，如 "local" "netease"。
	ID() string
	// Search 按关键词检索曲目。
	Search(ctx context.Context, query string) ([]Track, error)
	// GetTrack 获取单条元数据。
	GetTrack(ctx context.Context, ref TrackRef) (Track, error)
	// Resolve 将逻辑引用兑换为可拉流的物理地址。
	Resolve(ctx context.Context, ref TrackRef) (StreamLocator, error)
}

// TrackSource 是曲目源抽象：radio 模式的供给端。
// 实现方可以是通用歌单（DB 游标）、ncm 每日推荐（TTL 物化）、
// 相似歌曲/心动模式（链式种子）、私人FM（无限流）等。
type TrackSource interface {
	// NextBatch 取下一批曲目（至多 n 首）。
	// seed 为房间当前播放曲目的 ref（无播放时为零值），链式源用它换批。
	// exhausted=true 表示源耗尽（仅有限源在 once 语义下会为 true）。
	NextBatch(ctx context.Context, n int, seed TrackRef) (tracks []Track, exhausted bool, err error)
	// Description 展示用描述，如 "歌单《古典精选》" / "网易云每日推荐"。
	Description() string
	// Finite 有限源返回 true（接受 shuffle/once 语义）；无限流返回 false。
	Finite() bool
}

// SourceFactory 是可选接口：能充当曲目源工厂的 Provider 实现它。
// spec 为 "<provider>:" 之后的部分（如 "daily" "fm" "simi:<id>"）。
type SourceFactory interface {
	Provider
	NewSource(ctx context.Context, spec string) (TrackSource, error)
}

// PlaylistImporter 是可选接口：支持导入外部歌单的 Provider 实现它。
type PlaylistImporter interface {
	Provider
	// ImportPlaylist 拉取外部歌单全量曲目；playlistID 可为裸 id 或完整 URL。
	ImportPlaylist(ctx context.Context, playlistID string) (name string, tracks []Track, err error)
}

// QRLoginAware 是可选接口：支持二维码登录的 Provider 实现它。
// status 取值：expired | waiting | scanned | ok。
type QRLoginAware interface {
	Provider
	QRLoginStart(ctx context.Context) (key, qrContent string, err error)
	QRLoginPoll(ctx context.Context, key string) (status, message string, err error)
}

// CredentialAware 是可选接口：支持凭据热更新的 Provider 实现它。
type CredentialAware interface {
	Provider
	// SetCredential 校验并热更新凭据；校验失败必须返回错误且不生效。
	SetCredential(ctx context.Context, payload string) error
	// CredentialStatus 返回 unset | ok | invalid。
	CredentialStatus(ctx context.Context) string
}

// Registry 是 provider 注册表。
type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }

func (r *Registry) Register(p Provider) { r.providers[p.ID()] = p }

func (r *Registry) Get(id string) (Provider, bool) {
	p, ok := r.providers[id]
	return p, ok
}

// ForRef 按 TrackRef 前缀查找 provider。
func (r *Registry) ForRef(ref TrackRef) (Provider, string, error) {
	pid, id, err := ref.Split()
	if err != nil {
		return nil, "", err
	}
	p, ok := r.providers[pid]
	if !ok {
		return nil, "", fmt.Errorf("unknown provider %q", pid)
	}
	return p, id, nil
}

func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.providers))
	for id := range r.providers {
		out = append(out, id)
	}
	return out
}

// All 返回全部已注册 provider。
func (r *Registry) All() []Provider {
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	return out
}
