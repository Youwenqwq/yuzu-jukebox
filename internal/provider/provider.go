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
	"errors"
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

// Contributor 创作贡献者。Role 约定取值：
// "artist"(歌手/出演) "composer"(作曲) "lyricist"(作词)
// "uploader"(上传者/UP主)。provider 尽力而为，可为空。
type Contributor struct {
	Role string `json:"role"`
	Name string `json:"name"`
}

// Track 是媒体元数据（曲目层）。Album/CoverURL/SourceURL/Contributors
// 为可选富字段，provider 按数据源实际能力填充，客户端对空值降级。
// CoverURL 可能是服务端代理地址（源站需 Referer 时）。
type Track struct {
	Ref          TrackRef      `json:"track_ref"`
	Title        string        `json:"title"`
	Artist       string        `json:"artist"`
	DurationMs   int64         `json:"duration_ms"`
	Album        string        `json:"album,omitempty"`
	CoverURL     string        `json:"cover_url,omitempty"`
	SourceURL    string        `json:"source_url,omitempty"`
	Contributors []Contributor `json:"contributors,omitempty"`
}

// Lyrics 歌词。Type 目前只有 "lrc"；TLRC 为翻译（可无）。
type Lyrics struct {
	Type string `json:"type"`
	LRC  string `json:"lrc"`
	TLRC string `json:"tlrc,omitempty"`
}

// ErrNotSupported 能力缺席：provider 明确不支持某项可选能力。
var ErrNotSupported = errors.New("capability not supported by provider")

// SearchCategory 分类检索轴。
type SearchCategory string

const (
	SearchCategorySong     SearchCategory = "song"
	SearchCategoryArtist   SearchCategory = "artist"
	SearchCategoryAlbum    SearchCategory = "album"
	SearchCategoryPlaylist SearchCategory = "playlist"
)

// SearchResult 分类检索结果：判别实体。
// Type=song 时 Track 非空（可直接入队）；其余类型为实体，
// EntityID 用于钻取（EntityTracks：artist/album）或导入
// （playlist：走 PlaylistImporter 流程，不经 EntityTracks）。
type SearchResult struct {
	Type     SearchCategory `json:"type"`
	Track    *Track         `json:"track,omitempty"`
	EntityID string         `json:"entity_id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Detail   string         `json:"detail,omitempty"` // 次要文本：专辑歌手/歌单曲目数/UP主签名等
	CoverURL string         `json:"cover_url,omitempty"`
}

// CategorySearcher 是可选接口：支持分类检索的 Provider 实现它。
// 不支持的 provider 直接不实现，调用方以类型断言探测；
// 能力经 /api/v1/providers 的 capabilities.search_categories 报告给客户端。
type CategorySearcher interface {
	Provider
	// SearchCategories 报告支持的分类（恒含 song）。
	SearchCategories() []SearchCategory
	// SearchCategory 按分类检索；cat 必须是 SearchCategories 报告的值。
	SearchCategory(ctx context.Context, cat SearchCategory, query string) ([]SearchResult, error)
	// EntityTracks 钻取：把 artist/album 实体展开为可入队曲目。
	// 不支持的组合（如 playlist）返回 ErrNotSupported。
	EntityTracks(ctx context.Context, cat SearchCategory, entityID string) ([]Track, error)
}

// LyricsProvider 是可选接口：能提供歌词的 Provider 实现它。
// 不支持的 provider 直接不实现，调用方以类型断言探测。
type LyricsProvider interface {
	Provider
	Lyrics(ctx context.Context, ref TrackRef) (Lyrics, error)
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
	// 物理层质量信息（随音质档位变化，故在 Locator 而非 Track）：
	SizeBytes   int64 // 0 = 未知
	BitrateKbps int   // 0 = 未知
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

// RadioSource 描述一个电台源规格的参数约束（spec 不含 provider 前缀）。
type RadioSource struct {
	Spec   string `json:"spec"`           // daily | fm | simi | heart
	Arg    string `json:"arg,omitempty"`  // 参数语义，如 "track_id"；空 = 无参
	Name   string `json:"name,omitempty"` // 展示名
	Finite bool   `json:"finite"`         // 有限源才允许 shuffle/once
}

// SourceCatalog 是可选接口：SourceFactory provider 实现它向客户端报告可用电台源。
// 不支持的 provider 直接不实现，调用方以类型断言探测。
type SourceCatalog interface {
	SourceFactory
	RadioSources() []RadioSource
}

// PlaylistImporter 是可选接口：支持导入外部歌单的 Provider 实现它。
type PlaylistImporter interface {
	Provider
	// ImportPlaylist 拉取外部歌单全量曲目；playlistID 可为裸 id 或完整 URL。
	ImportPlaylist(ctx context.Context, playlistID string) (name string, tracks []Track, err error)
}

// CoverAware 是可选接口：源站封面需要特定请求头（如 Referer）的
// Provider 实现它。封面代理由此取头。
type CoverAware interface {
	Provider
	CoverHeaders() http.Header
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

// PlayReporter 是可选接口：能把播放记录上报回凭据账号的 Provider 实现它
// （如 NCM scrobble）。上报是 fire-and-forget：调用方负责 owner 校验
// （RequestedBy == 凭据 owner）、短超时与失败降级；实现不得阻塞调用方。
type PlayReporter interface {
	Provider
	// ReportPlay 上报一次播放。id 为 TrackRef 的 id 段；
	// playedMs 为实际播放时长，totalMs 为曲目总时长（0 = 未知）。
	ReportPlay(ctx context.Context, id string, playedMs, totalMs int64) error
}

// AccountPlaylist 凭据账号下的歌单摘要（playlist_add 的目标选择）。
type AccountPlaylist struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CoverURL   string `json:"cover_url,omitempty"`
	TrackCount int    `json:"track_count"`
}

// AccountWriter 是可选接口：支持对凭据账号做写操作的 Provider 实现它。
// 授权（acting Principal == 凭据 owner）由调用方在 API 层完成；
// 实现只负责用内部凭据执行，凭据永不下发。
type AccountWriter interface {
	Provider
	// Like 将曲目加入"我喜欢的音乐"。id 为 TrackRef 的 id 段。
	Like(ctx context.Context, id string) error
	// LikeCheck 回读喜欢状态（❤ 的客户端显隐/填充）。
	LikeCheck(ctx context.Context, id string) (bool, error)
	// AddToPlaylist 将曲目加入凭据账号的指定歌单。
	AddToPlaylist(ctx context.Context, playlistID, trackID string) error
	// AccountPlaylists 列出凭据账号的歌单（加歌单 UI 的目标枚举）。
	AccountPlaylists(ctx context.Context) ([]AccountPlaylist, error)
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
