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
	"sort"
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
	Role     string `json:"role"`
	Name     string `json:"name"`
	EntityID string `json:"entity_id,omitempty"`
}

// Track 是媒体元数据（曲目层）。Album/CoverURL/SourceURL/Contributors/
// Description 为可选富字段，provider 按数据源实际能力填充，客户端对空值降级。
// Description 是曲目级简介（NCM 歌曲百科 / QQ intro / B 站视频简介），
// 只在零成本可得时填充（随元数据端点同响应返回），不进热路径。
// CoverURL 可能是服务端代理地址（源站需 Referer 时）。
type Track struct {
	Ref          TrackRef      `json:"track_ref"`
	Title        string        `json:"title"`
	Artist       string        `json:"artist"`
	DurationMs   int64         `json:"duration_ms"`
	Album        string        `json:"album,omitempty"`
	Description  string        `json:"description,omitempty"`
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
	// SearchCategory 按分类分页检索；cat 必须是 SearchCategories 报告的值。
	// limit <= 0 时默认 30，offset < 0 时按 0 处理，实现按上游上限收敛。
	SearchCategory(ctx context.Context, cat SearchCategory, query string, limit, offset int) ([]SearchResult, error)
	// EntityTracks 分页钻取：把 artist/album 实体展开为可入队曲目。
	// limit <= 0 时默认 30，offset < 0 时按 0 处理；不支持的组合
	// （如 playlist）返回 ErrNotSupported。
	EntityTracks(ctx context.Context, cat SearchCategory, entityID string, limit, offset int) ([]Track, error)
}

// EntityAlbumLister 是可选接口：能把歌手实体展开为专辑实体列表。
// 结果与分类检索的 album 实体同构（Type=album/EntityID/Name/Detail/CoverURL），
// 可再经 EntityTracks(album) 钻到曲目——保持钻取终点归一契约。
type EntityAlbumLister interface {
	Provider
	EntityAlbums(ctx context.Context, artistID string, limit, offset int) ([]SearchResult, error)
}

// SimilarQuerier 是可选接口：按 Provider 作用域内的裸曲目 ID
// 一次性查询相似曲目。它不创建 TrackSource，也不维护链式源状态。
type SimilarQuerier interface {
	Provider
	Similar(ctx context.Context, trackID string, limit int) ([]Track, error)
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
	// Search 按关键词分页检索曲目。limit <= 0 时默认 30，offset < 0
	// 时按 0 处理，实现按上游上限收敛。
	Search(ctx context.Context, query string, limit, offset int) ([]Track, error)
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

// TrackSourceTotaler 是可选接口：有限源在已知总曲目数时实现它。
// known=false 表示当前尚未知（例如首批上游请求前）；调用方不得猜测。
type TrackSourceTotaler interface {
	Total() (total int, known bool)
}

// SourceFactory 是可选接口：能充当曲目源工厂的 Provider 实现它。
// spec 为 "<provider>:" 之后的部分（如 "daily" "fm" "simi:<id>"）。
type SourceFactory interface {
	Provider
	NewSource(ctx context.Context, spec string) (TrackSource, error)
}

// RadioSource 描述一个电台源规格的参数约束（spec 不含 provider 前缀）。
type RadioSource struct {
	Spec               string `json:"spec"`                          // daily | fm | simi | heart
	Arg                string `json:"arg,omitempty"`                 // 参数语义，如 "track_id"；空 = 无参
	Name               string `json:"name,omitempty"`                // 展示名
	Finite             bool   `json:"finite"`                        // 有限源才允许 shuffle/once
	RequiresCredential bool   `json:"requires_credential,omitempty"` // true = 使用服务端配置凭据
}

// RadioSourceEntry 可枚举电台源目录条目（动态目录，如 QQ 榜单全集）。
// CoverURL 为源站原始 URL，httpapi 序列化层改写为代理路径。
type RadioSourceEntry struct {
	Spec     string `json:"spec"`                // 完整规格（不含 provider 前缀），如 "top:26"
	Name     string `json:"name"`                // 展示名
	CoverURL string `json:"cover_url,omitempty"` // 封面（源站原始 URL）
	Detail   string `json:"detail,omitempty"`    // 副行文本（更新周期/简介等）
}

// RadioSourceCatalogLister 是可选接口：报告可枚举的电台源目录。
// 静态 RadioSources() 之外的动态目录（如 QQ 榜单 top:<id> 全集）经
// /api/v1/providers/{id}/radio-catalog 下发；不实现则端点 501。
type RadioSourceCatalogLister interface {
	Provider
	RadioSourceCatalog(ctx context.Context) ([]RadioSourceEntry, error)
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
	// coverURL 为外部歌单封面（原始源站 URL，调用方负责代理改写）；无封面返回空串。
	ImportPlaylist(ctx context.Context, playlistID string) (name, coverURL string, tracks []Track, err error)
}

// CoverAware 是可选接口：源站封面需要特定请求头（如 Referer）的
// Provider 实现它。封面代理由此取头。
type CoverAware interface {
	Provider
	CoverHeaders() http.Header
}

// CoverMode 决定统一封面端点（/api/v1/cover/{ref}、/api/v1/cover/ext/{token}）
// 在源站可直连时的取图方式。
type CoverMode string

const (
	// CoverModeProxy 由服务器带回源头取图并回写响应。
	// 适用于客户端不可直连的源站（防盗链、需要 Referer 等请求头）。
	CoverModeProxy CoverMode = "proxy"
	// CoverModeRedirect 服务器 302 到源站 URL，由客户端直连。
	// 适用于无防盗链的图床，省服务器带宽。
	CoverModeRedirect CoverMode = "redirect"
)

// CoverModeAware 是可选接口：Provider 显式声明封面取图模式。
// 决策优先级（见 httpapi.Server.coverMode）：
//  1. CoverAware（需要 Referer 等请求头）→ 恒代理，302 会丢头；
//  2. 显式声明 CoverMode → 以声明为准；
//  3. 未声明 → 全局默认（配置 ncm.cover_direct）。
type CoverModeAware interface {
	Provider
	CoverMode() CoverMode
}

// CoverThumbnailer 由封面 CDN 支持尺寸变体的 provider 实现。
// 代理默认应用缩略图变换；客户端传 ?size=original 时跳过。
type CoverThumbnailer interface {
	Provider
	ThumbnailCoverURL(raw string) string
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

// ArtistDetail 是按名字解析出的 provider 歌手实体。EntityID 是该 Provider
// 的实体键；AvatarURL 为源站原始 URL，由 httpapi 序列化层改写为服务端
// 代理路径（与实体封面同一不变量）。
type ArtistDetail struct {
	Name      string `json:"name"`
	EntityID  string `json:"entity_id,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Bio       string `json:"bio,omitempty"`
}

// ArtistDetailer 是可选接口：能把艺人名解析为 provider 歌手实体。
// 实现方自行决定名字到实体 ID 的映射（如 NCM 先 type=100 搜索取首条）。
// 解析失败或名字不存在时返回错误，httpapi 会继续尝试其它 Provider。
type ArtistDetailer interface {
	Provider
	ArtistDetail(ctx context.Context, name string) (ArtistDetail, error)
}

// ArtistIDDetailer 是可选接口：按 Provider 原生实体 ID 直接解析歌手，
// 不经过名字搜索。
type ArtistIDDetailer interface {
	Provider
	ArtistDetailByID(ctx context.Context, entityID string) (ArtistDetail, error)
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

// All 返回全部已注册 provider，按 ID 稳定排序（调用方依赖确定性顺序，
// 如推荐 feed 聚合与艺人富化优先序、listProviders 的响应顺序）。
func (r *Registry) All() []Provider {
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}
