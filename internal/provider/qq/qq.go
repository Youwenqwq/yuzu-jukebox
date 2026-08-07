// Package qq 实现 QQ 音乐 Provider，后端为 QQMusicApi 的 FastAPI sidecar
// （https://github.com/L-1124/QQMusicApi，web/ 目录）。
//
// 凭据模型：QQ 的 Credential 是结构化 JSON 对象（musicid+musickey 核心对，
// 外加 refresh_token/encrypt_uin 等），序列化后存于 credentials 表；
// 请求时以 Cookie 头（musicid=...; musickey=...）回传 sidecar，支持热更新。
// 未配置凭据时搜索/详情/歌词可用，Resolve 只能拿 128kbps guest 档或失败。
//
// TrackRef 使用歌曲 mid（如 "qq:003aQmDc4dRTXQ"），与 bili 的 BV 号同策略：
// mid 是全端点通吃的稳定标识（detail/lyric/url 均接受），账号写操作所需的
// 数字 id 由 GetTrack 回读。
package qq

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png" // 注册 PNG 解码器（QR 图片）
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// 明文音质档位（file_type 枚举索引，0-16）。17+ 是加密格式（需 ekey 解密），
// 播放器无法直接拉流，拒绝配置。默认 MP3_320。
const (
	fileTypeFLAC   = 7
	fileTypeMP3320 = 12
	fileTypeMP3128 = 13
	fileTypeACC192 = 14

	defaultFileType = fileTypeMP3320
)

// fallbackCDNHost 是 SDK 内置的兜底 CDN 根地址（get_cdn_dispatch 不可用时）。
// 与 SDK 的 _SONG_URL_FALLBACK_DOMAIN 一致，保留尾斜杠（purl 无前导斜杠）。
const fallbackCDNHost = "https://isure.stream.qqmusic.qq.com/"

// cdnHostTTL 缓存 CDN 调度结果的时长；调度结果本身带 cache_time/refresh_time，
// 这里取一个保守的固定值。
const cdnHostTTL = 30 * time.Minute

// 二维码登录事件码（web /login/qrcode/{type}/status → event）。
const (
	qrEventDone    = 0
	qrEventScan    = 1
	qrEventConfirm = 2
	qrEventTimeout = 3
	qrEventRefuse  = 4
)

type Provider struct {
	base     string
	fileType int
	st       *store.Store

	client      *http.Client // 常规请求
	writeClient *http.Client // 账号写操作（短超时）

	cred atomicValue[qqCredential] // 最新凭据；nil = 未配置

	mu         sync.Mutex
	cdnRoot    string
	cdnFetched time.Time
}

var (
	_ provider.CredentialAware  = (*Provider)(nil)
	_ provider.QRLoginAware     = (*Provider)(nil)
	_ provider.LyricsProvider   = (*Provider)(nil)
	_ provider.CategorySearcher = (*Provider)(nil)
	_ provider.CoverThumbnailer = (*Provider)(nil)
	_ provider.SourceCatalog    = (*Provider)(nil)
	_ provider.PlaylistImporter = (*Provider)(nil)
	_ provider.AccountWriter    = (*Provider)(nil)
)

// atomicValue 是 atomic.Value 的窄封装（存 nil 指针安全）。
type atomicValue[T any] struct {
	v atomic.Value
}

func (a *atomicValue[T]) Load() *T {
	v, _ := a.v.Load().(*T)
	return v
}

func (a *atomicValue[T]) Store(p *T) { a.v.Store(p) }

// New 创建 QQ Provider。fileType 为明文音质档位（0-16，默认 12=MP3_320）；
// 非法值回退默认。启动时从 DB 恢复凭据。
func New(baseURL string, fileType int, st *store.Store) *Provider {
	if fileType < 0 || fileType > 16 {
		fileType = defaultFileType
	}
	p := &Provider{
		base:        strings.TrimRight(baseURL, "/"),
		fileType:    fileType,
		st:          st,
		client:      &http.Client{Timeout: 15 * time.Second},
		writeClient: &http.Client{Timeout: 8 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st != nil {
		if payload, err := st.GetCredential(ctx, p.ID()); err == nil && payload != "" {
			if cred, err := parseCredential(payload); err == nil {
				p.cred.Store(cred)
			}
		}
	}
	return p
}

func (p *Provider) ID() string { return "qq" }

// ThumbnailCoverURL implements provider.CoverThumbnailer as a no-op: QQ cover
// URLs are already size-bounded by construction (T00xR300x300M000), so the
// cover proxy's ?size=original escape hatch has no visible effect for QQ.
func (*Provider) ThumbnailCoverURL(raw string) string { return raw }

// ---------- 凭据（provider.CredentialAware） ----------

// qqCredential 是 sidecar 的登录凭据对象。序列化字段名即 sidecar Cookie
// 契约（snake_case），与 auth.py 的 cookie 读取完全一致；
// 响应侧 alias 字段（musickeyCreateTime 等 camelCase）由 UnmarshalJSON 归一。
type qqCredential struct {
	OpenID             string `json:"openid"`
	RefreshToken       string `json:"refresh_token"`
	AccessToken        string `json:"access_token"`
	ExpiredAt          int64  `json:"expired_at"`
	MusicID            int64  `json:"musicid"`
	MusicKey           string `json:"musickey"`
	UnionID            string `json:"unionid"`
	StrMusicID         string `json:"str_musicid"`
	RefreshKey         string `json:"refresh_key"`
	MusicKeyCreateTime int64  `json:"musickey_create_time"`
	KeyExpiresIn       int64  `json:"key_expires_in"`
	FirstLogin         int    `json:"first_login"`
	BindAccountType    int    `json:"bind_account_type"`
	NeedRefreshKeyIn   int64  `json:"need_refresh_key_in"`
	EncryptUin         string `json:"encrypt_uin"`
	LoginType          int    `json:"login_type"`
}

// UnmarshalJSON 把响应侧的 camelCase alias（musickeyCreateTime 等）归一为
// snake_case 字段名，保证 QR 登录返回的凭据可直接解析。
func (c *qqCredential) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	norm := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		norm[camelToSnake(k)] = v
	}
	out, err := json.Marshal(norm)
	if err != nil {
		return err
	}
	type alias qqCredential
	return json.Unmarshal(out, (*alias)(c))
}

// camelToSnake 把 camelCase 键转成 snake_case（仅用于凭据字段归一）。
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hasLogin 对应 sidecar 的 credential_has_login：musicid>0 且 musickey 非空。
func (c *qqCredential) hasLogin() bool {
	return c != nil && c.MusicID > 0 && c.MusicKey != ""
}

// cookieHeader 把凭据序列化为 sidecar 的 Cookie 头（字段名即 auth.py 读取项）。
func (c *qqCredential) cookieHeader() string {
	parts := []string{
		"musicid=" + strconv.FormatInt(c.MusicID, 10),
		"musickey=" + c.MusicKey,
	}
	for _, kv := range [][2]string{
		{"openid", c.OpenID},
		{"refresh_token", c.RefreshToken},
		{"access_token", c.AccessToken},
		{"unionid", c.UnionID},
		{"str_musicid", c.StrMusicID},
		{"refresh_key", c.RefreshKey},
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if c.ExpiredAt > 0 {
		parts = append(parts, "expired_at="+strconv.FormatInt(c.ExpiredAt, 10))
	}
	return strings.Join(parts, "; ")
}

// parseCredential 解析凭据 payload：JSON 对象（QR/API 返回）或
// "musicid=...; musickey=..." Cookie 串（管理端手工粘贴）。
func parseCredential(payload string) (*qqCredential, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, fmt.Errorf("empty credential")
	}
	var cred qqCredential
	if strings.HasPrefix(payload, "{") {
		if err := json.Unmarshal([]byte(payload), &cred); err != nil {
			return nil, fmt.Errorf("invalid credential JSON: %w", err)
		}
	} else {
		for part := range strings.SplitSeq(payload, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "musicid":
				id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("invalid musicid in credential")
				}
				cred.MusicID = id
			case "musickey":
				cred.MusicKey = strings.TrimSpace(v)
			case "openid":
				cred.OpenID = strings.TrimSpace(v)
			case "refresh_token":
				cred.RefreshToken = strings.TrimSpace(v)
			case "access_token":
				cred.AccessToken = strings.TrimSpace(v)
			case "expired_at":
				cred.ExpiredAt, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			case "unionid":
				cred.UnionID = strings.TrimSpace(v)
			case "str_musicid":
				cred.StrMusicID = strings.TrimSpace(v)
			case "refresh_key":
				cred.RefreshKey = strings.TrimSpace(v)
			case "encrypt_uin":
				cred.EncryptUin = strings.TrimSpace(v)
			}
		}
	}
	if !cred.hasLogin() {
		return nil, fmt.Errorf("credential missing musicid/musickey")
	}
	if cred.StrMusicID == "" {
		cred.StrMusicID = strconv.FormatInt(cred.MusicID, 10)
	}
	return &cred, nil
}

// SetCredential 校验并热更新凭据；校验失败必须返回错误且不生效。
//
// 校验策略（sidecar 只能靠 refresh_credential 区分真伪——check_expired 对
// 垃圾 musickey 也返回 null/false，get_vip_info 同样放行）：
//   - 结构校验：musicid>0 且 musickey 非空。
//   - 携带 refresh 材料（refresh_token/refresh_key，QR 登录必有）时，
//     强制 refresh 探针：失败即拒绝，成功则直接存储刷新后的凭据（最鲜）。
//   - 仅有 musicid+musickey 的管理端粘贴凭据：无法在线鉴别，结构校验后接受；
//     若实际无效，会在 Resolve/CredentialStatus 阶段暴露。
func (p *Provider) SetCredential(ctx context.Context, payload string) error {
	cred, err := parseCredential(payload)
	if err != nil {
		return err
	}
	if cred.hasRefreshMaterial() {
		refreshed, err := p.refreshCredential(ctx, cred)
		if err != nil {
			return fmt.Errorf("credential validation failed: %w", err)
		}
		cred = refreshed
	}
	canonical, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	if err := p.st.UpsertCredential(ctx, p.ID(), string(canonical), "ok"); err != nil {
		return err
	}
	_ = p.st.SetCredentialAccount(ctx, p.ID(), p.accountProfile(ctx, cred))
	p.cred.Store(cred)
	return nil
}

// CredentialStatus 返回 unset | ok | invalid。
// 先按本地 key 字段判过期（musickey_create_time + key_expires_in）；
// 过期且可刷新则 refresh 续期并持久化；无刷新材料或刷新失败为 invalid。
// 本地无法判定时以 check_expired 兜底（弱信号，仅供刷新触发）。
func (p *Provider) CredentialStatus(ctx context.Context) string {
	cred := p.cred.Load()
	if !cred.hasLogin() {
		return "unset"
	}
	if cred.refreshable() && cred.locallyExpired() {
		if refreshed, err := p.refreshCredential(ctx, cred); err == nil {
			if p.persistCredential(ctx, refreshed) {
				return "ok"
			}
		}
		return "invalid"
	}
	expired, err := p.checkExpired(ctx, cred)
	if err != nil {
		return "invalid"
	}
	if !expired {
		return "ok"
	}
	if !cred.refreshable() {
		return "invalid"
	}
	refreshed, err := p.refreshCredential(ctx, cred)
	if err != nil {
		return "invalid"
	}
	if !p.persistCredential(ctx, refreshed) {
		return "invalid"
	}
	return "ok"
}

// persistCredential 把刷新后的凭据落盘并热生效。
func (p *Provider) persistCredential(ctx context.Context, cred *qqCredential) bool {
	canonical, err := json.Marshal(cred)
	if err != nil {
		return false
	}
	if err := p.st.UpsertCredential(ctx, p.ID(), string(canonical), "ok"); err != nil {
		return false
	}
	p.cred.Store(cred)
	return true
}

// hasRefreshMaterial 是否有 refresh_token/refresh_key 任一。
func (c *qqCredential) hasRefreshMaterial() bool {
	return c != nil && (c.RefreshToken != "" || c.RefreshKey != "")
}

// refreshable 同 hasRefreshMaterial（可尝试 refresh 续期）。
func (c *qqCredential) refreshable() bool { return c.hasRefreshMaterial() }

// locallyExpired 按本地 key 字段判过期（对应 sidecar credential_needs_refresh）。
func (c *qqCredential) locallyExpired() bool {
	if c == nil || c.MusicKeyCreateTime <= 0 || c.KeyExpiresIn <= 0 {
		return false
	}
	return time.Now().Unix() >= c.MusicKeyCreateTime+c.KeyExpiresIn
}

// checkExpired 调 /login/check_expired；返回 true 表示凭据已过期。
func (p *Provider) checkExpired(ctx context.Context, cred *qqCredential) (bool, error) {
	var expired bool
	if err := p.get(ctx, p.client, "/login/check_expired", nil, cred, &expired); err != nil {
		return false, err
	}
	return expired, nil
}

// refreshCredential 调 /login/refresh_credential 续期并返回新凭据。
func (p *Provider) refreshCredential(ctx context.Context, cred *qqCredential) (*qqCredential, error) {
	var data qqCredential
	if err := p.get(ctx, p.writeClient, "/login/refresh_credential", nil, cred, &data); err != nil {
		return nil, err
	}
	if !data.hasLogin() {
		return nil, fmt.Errorf("qq refresh: response missing musicid/musickey")
	}
	if data.StrMusicID == "" {
		data.StrMusicID = strconv.FormatInt(data.MusicID, 10)
	}
	return &data, nil
}

// accountProfile 尽力填充凭据账号资料快照（头像/昵称经 /user/{euin}/homepage）。
func (p *Provider) accountProfile(ctx context.Context, cred *qqCredential) store.AccountProfile {
	profile := store.AccountProfile{UID: cred.StrMusicID}
	if cred.EncryptUin == "" {
		return profile
	}
	var data struct {
		BaseInfo struct {
			Name   string `json:"name"`
			Avatar string `json:"avatar"`
		} `json:"base_info"`
	}
	if err := p.get(ctx, p.client, "/user/"+url.PathEscape(cred.EncryptUin)+"/homepage", nil, cred, &data); err != nil {
		return profile
	}
	profile.Name = data.BaseInfo.Name
	profile.Avatar = data.BaseInfo.Avatar
	return profile
}

// ---------- 二维码登录（provider.QRLoginAware） ----------

// QRLoginStart 生成二维码：返回 identifier（轮询用）与扫码内容文本（渲染二维码用）。
//
// QQ 上游与 ncm/bili 不同：sidecar 只返回**已编码的二维码 PNG**，扫码内容
// （腾讯的 txz.qq.com 授权 URL）从不以文本形式暴露，qrsig/identifier 只是
// 轮询键。yuzu 客户端按 ncm/bili 的契约把 qrContent 当文本编码成二维码，
// 因此这里用 gozxing 解码 PNG 还原内容；解码失败（上游异常）报错让用户重试。
func (p *Provider) QRLoginStart(ctx context.Context) (key, qrContent string, err error) {
	var data struct {
		Identifier string `json:"identifier"`
		Data       string `json:"data"` // base64 PNG
		Img        string `json:"img"`  // data URL（备用）
	}
	if err := p.get(ctx, p.client, "/login/qrcode/qq", nil, nil, &data); err != nil {
		return "", "", err
	}
	if data.Identifier == "" {
		return "", "", fmt.Errorf("qq qr: empty identifier")
	}
	content, err := decodeQRContent(data.Data)
	if err != nil {
		// img 是同一张图的 Data URL；data 缺省时兜底尝试
		if imgData, ok := strings.CutPrefix(data.Img, "data:image/png;base64,"); ok {
			content, err = decodeQRContent(imgData)
		}
		if err != nil {
			return "", "", fmt.Errorf("qq qr: decode content: %w", err)
		}
	}
	return data.Identifier, content, nil
}

// decodeQRContent 把 base64 编码的二维码 PNG 解码为内容文本。
// 兼容 "data:image/png;base64," 前缀（防御性）。
func decodeQRContent(b64 string) (string, error) {
	b64 = strings.TrimSpace(b64)
	b64, _ = strings.CutPrefix(b64, "data:image/png;base64,")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("invalid base64: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("invalid image: %w", err)
	}
	bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", err
	}
	result, err := qrcode.NewQRCodeReader().DecodeWithoutHints(bitmap)
	if err != nil {
		return "", fmt.Errorf("not a readable QR: %w", err)
	}
	text := result.GetText()
	if text == "" {
		return "", fmt.Errorf("empty QR content")
	}
	return text, nil
}

// QRLoginPoll 轮询扫码状态。status 为 expired|waiting|scanned|ok；
// ok 时凭据已被提取、校验并热生效。
func (p *Provider) QRLoginPoll(ctx context.Context, key string) (string, string, error) {
	var data struct {
		Event      int             `json:"event"`
		Done       bool            `json:"done"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := p.get(ctx, p.client, "/login/qrcode/qq/status",
		url.Values{"identifier": {key}}, nil, &data); err != nil {
		return "", "", err
	}
	switch data.Event {
	case qrEventDone:
		if len(data.Credential) == 0 {
			return "", "", fmt.Errorf("qq qr: done without credential")
		}
		cred, err := parseCredential(string(data.Credential))
		if err != nil {
			return "", "", err
		}
		canonical, err := json.Marshal(cred)
		if err != nil {
			return "", "", err
		}
		if err := p.SetCredential(ctx, string(canonical)); err != nil {
			return "", "", err
		}
		return "ok", "登录成功，凭据已生效", nil
	case qrEventScan:
		return "waiting", "等待扫码", nil
	case qrEventConfirm:
		return "scanned", "已扫码，请在手机上确认", nil
	case qrEventTimeout:
		return "expired", "二维码已过期", nil
	case qrEventRefuse:
		return "", "", fmt.Errorf("qq qr: login refused")
	default:
		return "", "", fmt.Errorf("qq qr: unexpected event %d", data.Event)
	}
}

// ---------- Provider 接口 ----------

// normalizeSearchPage 收敛 limit/offset 契约（同 ncm）。
func normalizeSearchPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 30
	} else if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// pageParams 把 limit/offset 换算成上游的 page/num（偏移不对齐时本地丢弃余数）。
func pageParams(limit, offset int) (num, page, drop int) {
	limit, offset = normalizeSearchPage(limit, offset)
	num = limit
	if num < 1 {
		num = 1
	}
	page = offset/num + 1
	drop = offset % num
	return num, page, drop
}

// Search 按关键词分页检索曲目（search_type=0）。
func (p *Provider) Search(ctx context.Context, query string, limit, offset int) ([]provider.Track, error) {
	num, page, drop := pageParams(limit, offset)
	q := url.Values{
		"keyword":     {query},
		"search_type": {"0"},
		"page":        {strconv.Itoa(page)},
		"num":         {strconv.Itoa(num)},
		"highlight":   {"false"},
	}
	var data struct {
		Song []qqSong `json:"song"`
	}
	if err := p.get(ctx, p.client, "/search/search_by_type", q, nil, &data); err != nil {
		return nil, err
	}
	songs := slicePage(data.Song, drop, num)
	out := make([]provider.Track, 0, len(songs))
	for _, s := range songs {
		out = append(out, p.songTrack(s))
	}
	return out, nil
}

// songDetail 取单曲详情（数字 id 与 type 也在这里回读，供账号写操作使用）。
func (p *Provider) songDetail(ctx context.Context, mid string) (qqSong, error) {
	var data struct {
		Track qqSong `json:"track"`
	}
	if err := p.get(ctx, p.client, "/song/"+url.PathEscape(mid)+"/detail", nil, nil, &data); err != nil {
		return qqSong{}, err
	}
	if data.Track.Mid == "" {
		return qqSong{}, fmt.Errorf("track not found: %s", mid)
	}
	return data.Track, nil
}

func (p *Provider) GetTrack(ctx context.Context, ref provider.TrackRef) (provider.Track, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.Track{}, err
	}
	song, err := p.songDetail(ctx, id)
	if err != nil {
		return provider.Track{}, err
	}
	track := p.songTrack(song)
	track.Ref = ref
	return track, nil
}

// Resolve 兑换可拉流地址。音质链：[配置档, MP3_320, MP3_128]，去重后逐级尝试；
// 无凭据时 320 档大概率拿不到，最终 128 档或报错。
func (p *Provider) Resolve(ctx context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.StreamLocator{}, err
	}
	cred := p.cred.Load()
	for _, ft := range p.fileTypeChain() {
		loc, ok, err := p.resolveType(ctx, id, ft, cred)
		if err != nil {
			return provider.StreamLocator{}, err
		}
		if ok {
			return loc, nil
		}
	}
	return provider.StreamLocator{}, fmt.Errorf(
		"no playable url for %s (可能需要登录或会员，请配置 qq 凭据)", ref)
}

// fileTypeChain 返回逐级尝试的音质链（去重）。
func (p *Provider) fileTypeChain() []int {
	chain := make([]int, 0, 3)
	for _, ft := range []int{p.fileType, fileTypeMP3320, fileTypeMP3128} {
		if ft >= 0 && ft <= 16 {
			chain = append(chain, ft)
		}
	}
	return dedupInts(chain)
}

func dedupInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// resolveType 用指定音质档请求播放地址；result!=0 或 purl 为空视为该档不可用。
func (p *Provider) resolveType(ctx context.Context, mid string, fileType int, cred *qqCredential) (provider.StreamLocator, bool, error) {
	body, err := json.Marshal(map[string]any{
		"file_info": []map[string]any{{"mid": mid}},
		"file_type": fileType,
	})
	if err != nil {
		return provider.StreamLocator{}, false, err
	}
	var data struct {
		Expiration int `json:"expiration"`
		Data       []struct {
			Mid    string `json:"mid"`
			Purl   string `json:"purl"`
			Result int    `json:"result"`
		} `json:"data"`
	}
	if err := p.postJSON(ctx, p.client, "/song/get_song_urls", body, cred, &data); err != nil {
		return provider.StreamLocator{}, false, err
	}
	if len(data.Data) == 0 || data.Data[0].Result != 0 || data.Data[0].Purl == "" {
		return provider.StreamLocator{}, false, nil
	}
	host, err := p.cdnHost(ctx)
	if err != nil {
		return provider.StreamLocator{}, false, err
	}
	loc := provider.StreamLocator{
		URL:         host + data.Data[0].Purl,
		Format:      fileTypeFormat(fileType),
		BitrateKbps: fileTypeBitrate(fileType),
	}
	if data.Expiration > 0 {
		loc.ExpiresAt = time.Now().Add(time.Duration(data.Expiration) * time.Second)
	}
	return loc, true, nil
}

// cdnHost 取可用的 CDN 根地址。30 分钟缓存；调度失败时回退旧缓存或 SDK
// 内置兜底域名。
//
// 拼接契约（与 SDK 的 cdn + purl 直拼一致）：sip 条目**保留尾斜杠**
// （如 "http://106.119.86.89/amobile.music.tc.qq.com/"），purl 无前导斜杠
// （"M800xxx.mp3?guid=..."）。去掉尾斜杠会把路径前缀的分隔符吃掉，
// 拼出 "…/amobile.music.tc.qq.comM800…" 畸形 URL——腾讯 CDN 返回 418。
func (p *Provider) cdnHost(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cdnRoot != "" && time.Since(p.cdnFetched) < cdnHostTTL {
		return p.cdnRoot, nil
	}
	var data struct {
		Retcode int      `json:"retcode"`
		Sip     []string `json:"sip"`
	}
	if err := p.get(ctx, p.client, "/song/get_cdn_dispatch", nil, nil, &data); err != nil {
		if p.cdnRoot != "" {
			return p.cdnRoot, nil // 旧缓存兜底
		}
		return "", err
	}
	for _, h := range data.Sip {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
			h = "https://" + h
		}
		p.cdnRoot, p.cdnFetched = h, time.Now()
		return h, nil
	}
	if p.cdnRoot != "" {
		return p.cdnRoot, nil
	}
	return fallbackCDNHost, nil
}

// ---------- 歌词（provider.LyricsProvider） ----------

// Lyrics 实现 provider.LyricsProvider：/song/{mid}/lyric → LRC 原文 + 翻译。
func (p *Provider) Lyrics(ctx context.Context, ref provider.TrackRef) (provider.Lyrics, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.Lyrics{}, err
	}
	var data struct {
		Lyric string `json:"lyric"`
		Trans string `json:"trans"`
	}
	if err := p.get(ctx, p.client, "/song/"+url.PathEscape(id)+"/lyric",
		url.Values{"trans": {"true"}}, nil, &data); err != nil {
		return provider.Lyrics{}, err
	}
	if data.Lyric == "" {
		return provider.Lyrics{}, fmt.Errorf("no lyrics for %s", ref)
	}
	return provider.Lyrics{Type: "lrc", LRC: data.Lyric, TLRC: data.Trans}, nil
}

// ---------- 曲目元数据 ----------

// qqSong 是 sidecar 序列化后的 Song 模型（字段名即 pydantic 字段名，
// 只取本 provider 需要的子集；上游缺省字段由 sidecar 兜底或忽略）。
type qqSong struct {
	ID       int64  `json:"id"`
	Mid      string `json:"mid"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
	Interval int    `json:"interval"` // 秒
	Singer   []struct {
		Mid  string `json:"mid"`
		Name string `json:"name"`
	} `json:"singer"`
	Album struct {
		Mid  string `json:"mid"`
		Name string `json:"name"`
		Pmid string `json:"pmid"`
	} `json:"album"`
}

// songTrack 把 qqSong 映射为 provider.Track。
func (p *Provider) songTrack(s qqSong) provider.Track {
	artists := make([]string, 0, len(s.Singer))
	contributors := make([]provider.Contributor, 0, len(s.Singer))
	for _, sg := range s.Singer {
		artists = append(artists, sg.Name)
		contributors = append(contributors, provider.Contributor{Role: "artist", Name: sg.Name})
	}
	return provider.Track{
		Ref:          provider.NewRef(p.ID(), s.Mid),
		Title:        s.Name,
		Artist:       strings.Join(artists, "/"),
		DurationMs:   int64(s.Interval) * 1000,
		Album:        s.Album.Name,
		CoverURL:     albumCoverURL(s.Album.Mid, s.Album.Pmid),
		SourceURL:    "https://y.qq.com/n/ryqq/songDetail/" + s.Mid,
		Contributors: contributors,
	}
}

// albumCoverURL 按 SDK 的 photo_new 规则拼专辑封面（T002R300x300M000{albumMid}.jpg）。
func albumCoverURL(albumMid, pmid string) string {
	mid := albumMid
	if mid == "" {
		mid = pmid
	}
	if mid == "" {
		return ""
	}
	return "https://y.gtimg.cn/music/photo_new/T002R300x300M000" + mid + ".jpg"
}

// fileTypeFormat 音质档 → 容器格式（未知返回空串）。
func fileTypeFormat(ft int) string {
	switch ft {
	case fileTypeFLAC, 1, 2:
		return "flac"
	case 8, 9, 10, 11:
		return "ogg"
	case fileTypeMP3320, fileTypeMP3128:
		return "mp3"
	case fileTypeACC192, 15, 16:
		return "m4a"
	default:
		return ""
	}
}

// fileTypeBitrate 音质档 → 码率 kbps（物理元数据展示用）。
func fileTypeBitrate(ft int) int {
	switch ft {
	case fileTypeFLAC:
		return 900
	case 8:
		return 640
	case 9:
		return 320
	case 10:
		return 192
	case 11, 15:
		return 96
	case fileTypeMP3320:
		return 320
	case fileTypeMP3128:
		return 128
	case fileTypeACC192:
		return 192
	case 16:
		return 48
	default:
		return 0
	}
}

// slicePage 丢弃 drop 个前导项并截断到 limit（offset 不对齐时的本地收敛）。
func slicePage[T any](items []T, drop, limit int) []T {
	if drop >= len(items) {
		return nil
	}
	items = items[drop:]
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// ---------- 内部 HTTP ----------

// apiEnvelope 是 sidecar 的统一响应信封 {code, msg, data}。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// get 发 GET 请求并解码信封内的 data。cred 非空时以 Cookie 头传递凭据。
func (p *Provider) get(ctx context.Context, client *http.Client, path string, q url.Values, cred *qqCredential, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if cred != nil {
		req.Header.Set("Cookie", cred.cookieHeader())
	}
	return p.do(client, req, path, out)
}

// postJSON 发 JSON body 的 POST 请求（/song/get_song_urls 用）。
func (p *Provider) postJSON(ctx context.Context, client *http.Client, path string, body []byte, cred *qqCredential, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cred != nil {
		req.Header.Set("Cookie", cred.cookieHeader())
	}
	return p.do(client, req, path, out)
}

// do 执行请求并解析信封；HTTP 非 200 或业务 code!=0 均为错误。
func (p *Provider) do(client *http.Client, req *http.Request, path string, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("qq api %s: %w", path, err)
	}
	defer resp.Body.Close()
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("qq api %s: decode: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := env.Msg
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("qq api %s: HTTP %d (%s)", path, resp.StatusCode, msg)
	}
	if env.Code != 0 {
		msg := env.Msg
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("qq api %s: code %d (%s)", path, env.Code, msg)
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}
