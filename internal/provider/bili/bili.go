// Package bili 实现哔哩哔哩 Provider，后端为 ~/bilibili-api 的
// FastAPI sidecar（封装 WBI 签名、buvid3 风控、DASH 音频选轨）。
//
// 凭据模型：SESSDATA 等 cookie 串存于 credentials 表，经
// X-Yuzu-Bilibili-Cookie 头逐请求传给 sidecar，支持热更新。
// 无 cookie 时 Resolve 可用（匿名 320kbps 上限），Search 会被风控拒绝。
package bili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// 拉流时 B 站 CDN 校验的头（写死在 locator 里，缓存层取用）。
var streamHeaders = http.Header{
	"Referer":    {"https://www.bilibili.com/"},
	"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"},
}

const cookieHeader = "X-Yuzu-Bilibili-Cookie"

// B 站 CDN URL 约 120 分钟过期；留 10 分钟安全余量。
const streamURLTTL = 110 * time.Minute

type Provider struct {
	base   string
	st     *store.Store
	client *http.Client

	cookie atomic.Value // string，空串 = 未配置
}

func New(baseURL string, st *store.Store) *Provider {
	p := &Provider{
		base:   strings.TrimRight(baseURL, "/"),
		st:     st,
		client: &http.Client{Timeout: 20 * time.Second},
	}
	p.cookie.Store("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if payload, err := st.GetCredential(ctx, p.ID()); err == nil && payload != "" {
		p.cookie.Store(payload)
	}
	return p
}

func (p *Provider) ID() string { return "bili" }

// ---------- 凭据管理（provider.CredentialAware） ----------

// SetCredential 校验并热更新 cookie（"SESSDATA=...; bili_jct=..." 串）。
func (p *Provider) SetCredential(ctx context.Context, payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return fmt.Errorf("empty credential")
	}
	status, err := p.accountStatus(ctx, payload)
	if err != nil {
		_ = p.st.UpsertCredential(ctx, p.ID(), payload, "invalid")
		return fmt.Errorf("credential validation failed: %w", err)
	}
	if !status.LoggedIn {
		_ = p.st.UpsertCredential(ctx, p.ID(), payload, "invalid")
		return fmt.Errorf("credential validation failed: not logged in")
	}
	if err := p.st.UpsertCredential(ctx, p.ID(), payload, "ok"); err != nil {
		return err
	}
	p.cookie.Store(payload)
	return nil
}

func (p *Provider) CredentialStatus(ctx context.Context) string {
	payload, err := p.st.GetCredential(ctx, p.ID())
	if err != nil || payload == "" {
		return "unset"
	}
	status, err := p.accountStatus(ctx, payload)
	if err != nil || !status.LoggedIn {
		return "invalid"
	}
	return "ok"
}

type accountStatus struct {
	Configured bool   `json:"configured"`
	LoggedIn   bool   `json:"logged_in"`
	AccountID  string `json:"account_id"`
	Name       string `json:"name"`
}

func (p *Provider) accountStatus(ctx context.Context, cookie string) (accountStatus, error) {
	var out accountStatus
	err := p.get(ctx, "/account/status", url.Values{}, cookie, &out)
	return out, err
}

// ---------- 二维码登录（provider.QRLoginAware） ----------

func (p *Provider) QRLoginStart(ctx context.Context) (key, qrContent string, err error) {
	var out struct {
		URL       string `json:"url"`
		QRCodeKey string `json:"qrcode_key"`
	}
	if err := p.get(ctx, "/login/qr/generate", url.Values{}, "", &out); err != nil {
		return "", "", err
	}
	if out.QRCodeKey == "" || out.URL == "" {
		return "", "", fmt.Errorf("qr generate: empty key or url")
	}
	return out.QRCodeKey, out.URL, nil
}

func (p *Provider) QRLoginPoll(ctx context.Context, key string) (string, string, error) {
	var out struct {
		Status string `json:"status"` // waiting | confirming | expired | authorized
		Cookie string `json:"cookie"`
	}
	if err := p.get(ctx, "/login/qr/poll", url.Values{"qrcode_key": {key}}, "", &out); err != nil {
		return "", "", err
	}
	switch out.Status {
	case "waiting":
		return "waiting", "等待扫码", nil
	case "confirming":
		return "scanned", "已扫码，请在 App 上确认", nil
	case "expired":
		return "expired", "二维码已过期", nil
	case "authorized":
		if out.Cookie == "" {
			return "", "", fmt.Errorf("qr login succeeded but cookie missing")
		}
		if err := p.SetCredential(ctx, out.Cookie); err != nil {
			return "", "", err
		}
		return "ok", "登录成功，凭据已生效", nil
	default:
		return "", "", fmt.Errorf("qr poll: unexpected status %q", out.Status)
	}
}

// ---------- Provider 接口 ----------

type videoResult struct {
	Bvid       string `json:"bvid"`
	Title      string `json:"title"`
	Author     string `json:"author"`
	Cover      string `json:"cover"`     // 可能是协议相对 URL（//i0.hdslb.com/...）
	Partition  string `json:"partition"` // 仅搜索结果提供，作为 Album
	DurationMs int64  `json:"duration_ms"`
	Published  int64  `json:"published"` // UP 主投稿列表提供
}

type videoListResponse struct {
	Results []videoResult `json:"results"`
	Total   int           `json:"total"` // 仅收藏夹内容列表提供
}

func (p *Provider) Search(ctx context.Context, query string, limit, offset int) ([]provider.Track, error) {
	if limit <= 0 {
		limit = 30
	} else if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := url.Values{
		"keywords": {query},
		"limit":    {strconv.Itoa(limit)},
		"offset":   {strconv.Itoa(offset)},
	}
	var resp videoListResponse
	if err := p.get(ctx, "/search", q, p.cookie.Load().(string), &resp); err != nil {
		return nil, err
	}
	out := make([]provider.Track, 0, len(resp.Results))
	for _, r := range resp.Results {
		if track, ok := p.trackFromVideo(r); ok {
			out = append(out, track)
		}
	}
	return out, nil
}

func (p *Provider) trackFromVideo(v videoResult) (provider.Track, bool) {
	if v.Bvid == "" {
		return provider.Track{}, false
	}
	return provider.Track{
		Ref:          provider.NewRef(p.ID(), v.Bvid),
		Title:        v.Title,
		Artist:       v.Author,
		Album:        v.Partition,
		CoverURL:     normalizeCoverURL(v.Cover),
		SourceURL:    videoURL(v.Bvid),
		DurationMs:   v.DurationMs,
		Contributors: uploaderContributor(v.Author),
	}, true
}

// SearchCategories 实现 provider.CategorySearcher；B 站仅支持视频与 UP 主。
func (p *Provider) SearchCategories() []provider.SearchCategory {
	return []provider.SearchCategory{
		provider.SearchCategorySong,
		provider.SearchCategoryArtist,
	}
}

func (p *Provider) SearchCategory(ctx context.Context, cat provider.SearchCategory, query string, limit, offset int) ([]provider.SearchResult, error) {
	switch cat {
	case provider.SearchCategorySong:
		tracks, err := p.Search(ctx, query, limit, offset)
		if err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, len(tracks))
		for i := range tracks {
			results[i] = provider.SearchResult{
				Type:  provider.SearchCategorySong,
				Track: &tracks[i],
			}
		}
		return results, nil

	case provider.SearchCategoryArtist:
		var resp struct {
			Results []struct {
				Mid  int64  `json:"mid"`
				Name string `json:"name"`
				Face string `json:"face"`
				Fans int64  `json:"fans"`
				Sign string `json:"sign"`
			} `json:"results"`
		}
		if limit <= 0 {
			limit = 30
		} else if limit > 30 {
			limit = 30
		}
		if offset < 0 {
			offset = 0
		}
		q := url.Values{
			"keywords": {query},
			"limit":    {strconv.Itoa(limit)},
			"pn":       {strconv.Itoa(offset/limit + 1)},
		}
		if err := p.get(ctx, "/search/up", q, p.cookie.Load().(string), &resp); err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, 0, len(resp.Results))
		for _, r := range resp.Results {
			detail := r.Sign
			if detail == "" {
				// 简介缺失时仍展示有意义的次要信息。
				detail = strconv.FormatInt(r.Fans, 10) + " 粉丝"
			}
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryArtist,
				EntityID: strconv.FormatInt(r.Mid, 10),
				Name:     r.Name,
				Detail:   detail,
				CoverURL: normalizeCoverURL(r.Face),
			})
		}
		return results, nil

	default:
		return nil, provider.ErrNotSupported
	}
}

func (p *Provider) EntityTracks(ctx context.Context, cat provider.SearchCategory, entityID string, limit, offset int) ([]provider.Track, error) {
	if cat != provider.SearchCategoryArtist {
		return nil, provider.ErrNotSupported
	}
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}

	const (
		pageSize = 30
		maxPages = 100
	)
	capacity := limit
	if capacity > pageSize*maxPages {
		capacity = pageSize * maxPages
	}
	tracks := make([]provider.Track, 0, capacity)
	startPN := offset/pageSize + 1
	firstSkip := offset % pageSize
	for page := range maxPages {
		pn := startPN + page
		q := url.Values{
			"mid": {entityID},
			"pn":  {strconv.Itoa(pn)},
			"ps":  {strconv.Itoa(pageSize)},
		}
		var resp videoListResponse
		if err := p.get(ctx, "/space/videos", q, p.cookie.Load().(string), &resp); err != nil {
			return nil, err
		}
		videos := resp.Results
		if page == 0 && firstSkip < len(videos) {
			videos = videos[firstSkip:]
		} else if page == 0 {
			videos = nil
		}
		for _, video := range videos {
			if track, ok := p.trackFromVideo(video); ok {
				tracks = append(tracks, track)
				if len(tracks) == limit {
					return tracks, nil
				}
			}
		}
		if len(resp.Results) < pageSize {
			break
		}
	}
	return tracks, nil
}

// ImportPlaylist 把 B 站收藏夹 media_id 导入为普通歌单。
func (p *Provider) ImportPlaylist(ctx context.Context, playlistID string) (string, string, []provider.Track, error) {
	if !isDecimalID(playlistID) {
		return "", "", nil, fmt.Errorf("invalid bili favorite media_id %q: want digits", playlistID)
	}
	cookie := p.cookie.Load().(string)
	if strings.TrimSpace(cookie) == "" {
		return "", "", nil, fmt.Errorf("bili 收藏夹导入需要登录 cookie")
	}
	tracks, err := p.favoriteTracks(ctx, playlistID, cookie)
	if err != nil {
		return "", "", nil, err
	}
	// sidecar 响应不含收藏夹标题；HTTP 导入流程允许空名称并支持调用方覆盖。
	// 上游侧无封面路径；sidecar 补 /fav/folder/info 后可在此接上。
	return "", "", tracks, nil
}

// favoriteTracks 通过收藏夹分页接口物化全部可用曲目。
func (p *Provider) favoriteTracks(ctx context.Context, mediaID, cookie string) ([]provider.Track, error) {
	const (
		pageSize = 20
		maxItems = 1000 // B 站收藏夹单夹内容上限为 1000，封顶对齐实际上限而非任意防御值。
	)
	tracks := make([]provider.Track, 0, pageSize)
	processed := 0
	total := -1
	for pn := 1; processed < maxItems; pn++ {
		q := url.Values{
			"media_id": {mediaID},
			"pn":       {strconv.Itoa(pn)},
			"ps":       {strconv.Itoa(pageSize)},
		}
		var resp videoListResponse
		if err := p.get(ctx, "/fav/resource/list", q, cookie, &resp); err != nil {
			return nil, err
		}
		if total < 0 {
			total = resp.Total
		}

		take := len(resp.Results)
		if remaining := maxItems - processed; take > remaining {
			take = remaining
		}
		if remaining := total - processed; take > remaining {
			take = remaining
		}
		for _, video := range resp.Results[:take] {
			processed++
			if track, ok := p.trackFromVideo(video); ok {
				tracks = append(tracks, track)
			}
		}
		if processed >= total || processed >= maxItems || len(resp.Results) < pageSize {
			break
		}
	}
	return tracks, nil
}

// NewSource 创建有限电台源。spec 取值：fav:<media_id>（收藏夹，需登录）|
// ranking:<key|rid>（分区排行榜，匿名；key 取 music/kichiku/all 等分区 key
// 或裸 rid）。
func (p *Provider) NewSource(ctx context.Context, spec string) (provider.TrackSource, error) {
	kind, arg, found := strings.Cut(spec, ":")
	if !found {
		return nil, fmt.Errorf("unknown bili source %q (want fav:<media_id>|ranking:<partition>)", spec)
	}
	switch kind {
	case "fav":
		if !isDecimalID(arg) {
			return nil, fmt.Errorf("invalid bili favorite media_id %q: want digits", arg)
		}
		cookie := p.cookie.Load().(string)
		if strings.TrimSpace(cookie) == "" {
			return nil, fmt.Errorf("bili 收藏夹电台需要登录 cookie")
		}
		tracks, err := p.favoriteTracks(ctx, arg, cookie)
		if err != nil {
			return nil, err
		}
		return &listSource{desc: "B 站收藏夹 " + arg, tracks: tracks}, nil
	case "ranking":
		return p.rankingSource(ctx, arg)
	default:
		return nil, fmt.Errorf("unknown bili source %q (want fav:<media_id>|ranking:<partition>)", spec)
	}
}

// rankingSource 把分区排行榜物化为有限电台源：一次 GET /region-ranking
// （ranking/v2，免 WBI、匿名可用，top ~100 条）。arg 为数字时按裸 rid 传，
// 否则按分区 key/名称/别名传（sidecar resolve_ranking_partition 解析）。
func (p *Provider) rankingSource(ctx context.Context, arg string) (provider.TrackSource, error) {
	if strings.TrimSpace(arg) == "" {
		return nil, fmt.Errorf("ranking source requires a partition: bili:ranking:<music|kichiku|all|rid>")
	}
	q := url.Values{"type": {"all"}}
	if isDecimalID(arg) {
		q.Set("rid", arg)
	} else {
		q.Set("partition", arg)
	}
	var resp struct {
		Results []videoResult `json:"results"`
	}
	if err := p.get(ctx, "/region-ranking", q, p.cookie.Load().(string), &resp); err != nil {
		return nil, err
	}
	tracks := make([]provider.Track, 0, len(resp.Results))
	for _, v := range resp.Results {
		if track, ok := p.trackFromVideo(v); ok {
			tracks = append(tracks, track)
		}
	}
	return &listSource{desc: "B 站排行榜 " + arg, tracks: tracks}, nil
}

// RadioSources 报告 B 站支持的电台源。排行榜匿名可用（免 WBI），
// 收藏夹需登录 cookie。
func (p *Provider) RadioSources() []provider.RadioSource {
	return []provider.RadioSource{
		{Spec: "ranking", Arg: "music", Name: "音乐区排行榜", Finite: true},
		{Spec: "ranking", Arg: "kichiku", Name: "鬼畜区排行榜", Finite: true},
		{Spec: "ranking", Arg: "all", Name: "全站排行榜", Finite: true},
		{Spec: "fav", Arg: "media_id", Name: "收藏夹电台", Finite: true, RequiresCredential: true},
	}
}

// listSource 是一次物化的有限曲目列表源（收藏夹/排行榜共用）。
type listSource struct {
	mu     sync.Mutex
	desc   string
	tracks []provider.Track
	cursor int
}

func (s *listSource) Description() string { return s.desc }
func (s *listSource) Finite() bool        { return true }
func (s *listSource) Total() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tracks), true
}

func (s *listSource) NextBatch(_ context.Context, n int, _ provider.TrackRef) ([]provider.Track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || s.cursor >= len(s.tracks) {
		return nil, s.cursor >= len(s.tracks), nil
	}
	end := min(s.cursor+n, len(s.tracks))
	batch := s.tracks[s.cursor:end]
	s.cursor = end
	return batch, s.cursor >= len(s.tracks), nil
}

func isDecimalID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

var (
	_ provider.CategorySearcher = (*Provider)(nil)
	_ provider.CoverThumbnailer = (*Provider)(nil)
	_ provider.PlaylistImporter = (*Provider)(nil)
	_ provider.SourceFactory    = (*Provider)(nil)
	_ provider.SourceCatalog    = (*Provider)(nil)
)

// parseRef 拆解扩展 ref：bili:BVxxx?p=N。无 ?p 默认为第 1 P。
func parseRef(ref provider.TrackRef) (bvid string, page int, err error) {
	_, id, err := ref.Split()
	if err != nil {
		return "", 0, err
	}
	bvid, pageStr, _ := strings.Cut(id, "?p=")
	page = 1
	if pageStr != "" {
		if page, err = strconv.Atoi(pageStr); err != nil || page < 1 {
			return "", 0, fmt.Errorf("invalid page in ref %q", ref)
		}
	}
	return bvid, page, nil
}

func (p *Provider) GetTrack(ctx context.Context, ref provider.TrackRef) (provider.Track, error) {
	bvid, page, err := parseRef(ref)
	if err != nil {
		return provider.Track{}, err
	}
	var resp struct {
		Title  string `json:"title"`
		Author string `json:"author"`
		Cover  string `json:"cover"` // sidecar 现有字段；完整 https URL
		Pic    string `json:"pic"`   // 预留：上游 bilibili API 原生封面字段
		Desc   string `json:"desc"`  // 视频简介（sidecar 透传自 view API data.desc）
		TName  string `json:"tname"` // 预留：视频分区，作为 Album
		Owner  struct {
			Name string `json:"name"` // 预留：UP 主名
		} `json:"owner"`
		DurationMs int64 `json:"duration_ms"` // 顶层：所有分 P 总时长
		Pages      []struct {
			Page       int    `json:"page"`
			Part       string `json:"part"`
			DurationMs int64  `json:"duration_ms"`
		} `json:"pages"`
	}
	if err := p.get(ctx, "/detail", url.Values{"bvid": {bvid}}, "", &resp); err != nil {
		return provider.Track{}, err
	}

	// 分 P 选择：时长必须取自目标分 P（顶层 duration 是全集合计，
	// 直接用它会让房间的结束 timer 严重超调）。
	title := resp.Title
	album := resp.TName
	duration := resp.DurationMs
	if len(resp.Pages) > 0 {
		if page > len(resp.Pages) {
			return provider.Track{}, fmt.Errorf("%s has %d pages, page %d out of range", bvid, len(resp.Pages), page)
		}
		pg := resp.Pages[page-1]
		if pg.DurationMs > 0 {
			duration = pg.DurationMs
		}
		if len(resp.Pages) > 1 {
			// 合集即专辑：分 P 曲目 Album 用视频总标题，标题用分 P 名
			album = resp.Title
			if pg.Part != "" {
				title = pg.Part
			}
		}
	}
	// 封面：pic（上游原生）优先，回退 sidecar 现有的 cover。
	cover := resp.Pic
	if cover == "" {
		cover = resp.Cover
	}
	// Contributors：owner.name 缺省时回退 author（同为 UP 主名）。
	uploader := resp.Owner.Name
	if uploader == "" {
		uploader = resp.Author
	}
	sourceURL := videoURL(bvid)
	if page > 1 {
		sourceURL += "?p=" + strconv.Itoa(page)
	}
	return provider.Track{
		Ref:          ref,
		Title:        title,
		Artist:       resp.Author,
		Album:        album,
		Description:  resp.Desc,
		CoverURL:     normalizeCoverURL(cover),
		SourceURL:    sourceURL,
		DurationMs:   duration,
		Contributors: uploaderContributor(uploader),
	}, nil
}

func (p *Provider) Resolve(ctx context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	bvid, page, err := parseRef(ref)
	if err != nil {
		return provider.StreamLocator{}, err
	}
	var resp struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		DurationMs  int64  `json:"duration_ms"`
		// sidecar 的 size 字段名历史上为 "bytes"；同时兼容 "size"。
		Size      int64 `json:"size"`
		Bytes     int64 `json:"bytes"`
		Bandwidth int   `json:"bandwidth"` // bps
	}
	if err := p.get(ctx, "/audio-url", url.Values{
		"bvid": {bvid}, "page": {strconv.Itoa(page)},
	}, p.cookie.Load().(string), &resp); err != nil {
		return provider.StreamLocator{}, err
	}
	if resp.URL == "" {
		return provider.StreamLocator{}, fmt.Errorf("no playable url for %s (可能需要登录或视频不可用)", ref)
	}
	sizeBytes := resp.Size
	if sizeBytes == 0 {
		sizeBytes = resp.Bytes
	}
	return provider.StreamLocator{
		URL:         resp.URL,
		Header:      streamHeaders,
		Format:      "m4a", // content_type 恒为 audio/mp4
		DurationMs:  resp.DurationMs,
		SizeBytes:   sizeBytes,
		BitrateKbps: resp.Bandwidth / 1000, // bandwidth 单位 bps；0 = 未知
		ExpiresAt:   time.Now().Add(streamURLTTL),
	}, nil
}

// CoverHeaders 实现 provider.CoverAware：B 站图床（hdslb.com）无 Referer 会 403，
// 与拉流复用同一组头。
func (p *Provider) CoverHeaders() http.Header { return streamHeaders }

// CoverMode 实现 provider.CoverModeAware：B 站图床需 Referer（CoverAware），
// 客户端无法直连，声明为显式代理模式。
func (*Provider) CoverMode() provider.CoverMode { return provider.CoverModeProxy }

const defaultCoverThumbnailSuffix = "@672w_378h_1c.webp"

var coverVariantSuffix = regexp.MustCompile(`@\d+w_\d+h(?:_[^/]*)?(?:\.[^/]*)?$`)

// ThumbnailCoverURL asks Bilibili's image CDN for its standard video-cover
// thumbnail. Foreign hosts and URLs that already carry a dimension variant
// are deliberately left unchanged.
func (*Provider) ThumbnailCoverURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !isBilibiliImageHost(u.Hostname()) || coverVariantSuffix.MatchString(u.Path) {
		return raw
	}
	u.Path += defaultCoverThumbnailSuffix
	u.RawPath = ""
	return u.String()
}

func isBilibiliImageHost(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, ".hdslb.com") || strings.HasSuffix(host, ".biliimg.com")
}

// videoURL 返回视频页地址（Track.SourceURL）。
func videoURL(bvid string) string {
	return "https://www.bilibili.com/video/" + bvid
}

// normalizeCoverURL 把协议相对 URL（//i0.hdslb.com/...）补全为 https。
func normalizeCoverURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// uploaderContributor 组装 UP 主贡献者；名字为空时返回 nil（字段留空合法）。
func uploaderContributor(name string) []provider.Contributor {
	if name == "" {
		return nil
	}
	return []provider.Contributor{{Role: "uploader", Name: name}}
}

// ---------- 内部 ----------

// get 发 GET 请求并解码 JSON；cookie 非空时经 X-Yuzu-Bilibili-Cookie 头传递。
// sidecar 出错时返回 FastAPI 的 {"detail": "..."}。
func (p *Provider) get(ctx context.Context, path string, q url.Values, cookie string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if cookie != "" {
		req.Header.Set(cookieHeader, cookie)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("bili api %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Detail string `json:"detail"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Detail == "" {
			e.Detail = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("bili api %s: %s", path, e.Detail)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
