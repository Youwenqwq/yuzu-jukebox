// Package ncm 实现网易云音乐 Provider，后端为 NeteaseCloudMusicApi(Enhanced) 实例。
//
// 凭据模型：MUSIC_U cookie 存于 credentials 表（非配置文件），
// 支持热更新（SetCredential 校验后即时生效，无需重启）。
// 未配置 cookie 时搜索/详情可用，Resolve 只能拿到低音质或试听片段。
package ncm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type Provider struct {
	base        string
	level       string
	st          *store.Store
	client      *http.Client
	writeClient *http.Client

	cookie atomic.Value // string，空串 = 未配置
}

var (
	_ provider.PlayReporter      = (*Provider)(nil)
	_ provider.AccountWriter     = (*Provider)(nil)
	_ provider.SourceCatalog     = (*Provider)(nil)
	_ provider.CategorySearcher  = (*Provider)(nil)
	_ provider.EntityAlbumLister = (*Provider)(nil)
	_ provider.SimilarQuerier    = (*Provider)(nil)
	_ provider.CoverThumbnailer  = (*Provider)(nil)
)

func New(baseURL, level string, st *store.Store) *Provider {
	if level == "" {
		level = "exhigh"
	}
	p := &Provider{
		base:        strings.TrimRight(baseURL, "/"),
		level:       level,
		st:          st,
		client:      &http.Client{Timeout: 15 * time.Second},
		writeClient: &http.Client{Timeout: 8 * time.Second},
	}
	p.cookie.Store("")
	// 启动时从 DB 恢复凭据
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if payload, err := st.GetCredential(ctx, p.ID()); err == nil && payload != "" {
		p.cookie.Store(payload)
	}
	return p
}

func (p *Provider) ID() string { return "ncm" }

// ---------- 凭据管理（provider.CredentialAware） ----------

// SetCredential 校验并热更新 cookie。payload 为 "MUSIC_U=xxx"（或完整 cookie 串）。
func (p *Provider) SetCredential(ctx context.Context, payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return fmt.Errorf("empty credential")
	}
	account, err := p.checkLogin(ctx, payload)
	if err != nil {
		_ = p.st.UpsertCredential(ctx, p.ID(), payload, "invalid")
		return fmt.Errorf("credential validation failed: %w", err)
	}
	if err := p.st.UpsertCredential(ctx, p.ID(), payload, "ok"); err != nil {
		return err
	}
	_ = p.st.SetCredentialAccount(ctx, p.ID(), account)
	p.cookie.Store(payload)
	return nil
}

func (p *Provider) CredentialStatus(ctx context.Context) string {
	payload, err := p.st.GetCredential(ctx, p.ID())
	if err != nil || payload == "" {
		return "unset"
	}
	if _, err := p.checkLogin(ctx, payload); err != nil {
		return "invalid"
	}
	return "ok"
}

// checkLogin 用 /login/status 验证 cookie，并返回登录账号资料。
func (p *Provider) checkLogin(ctx context.Context, cookie string) (store.AccountProfile, error) {
	var resp struct {
		Data struct {
			Profile *struct {
				UserID    int64  `json:"userId"`
				Nickname  string `json:"nickname"`
				AvatarURL string `json:"avatarUrl"`
			} `json:"profile"`
			Account *struct {
				ID int64 `json:"id"`
			} `json:"account"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/login/status", url.Values{}, cookie, &resp); err != nil {
		return store.AccountProfile{}, err
	}

	var account store.AccountProfile
	if resp.Data.Profile != nil {
		account.UID = strconv.FormatInt(resp.Data.Profile.UserID, 10)
		account.Name = resp.Data.Profile.Nickname
		account.Avatar = resp.Data.Profile.AvatarURL
	}
	if (account.UID == "" || account.UID == "0") && resp.Data.Account != nil {
		account.UID = strconv.FormatInt(resp.Data.Account.ID, 10)
	}
	if account.UID == "" || account.UID == "0" {
		return store.AccountProfile{}, fmt.Errorf("not logged in")
	}
	return account, nil
}

// ---------- 账号写操作 ----------

// ReportPlay 通过 /scrobble/v1 上报播放记录（sourceid 可选），时间单位为秒。
func (p *Provider) ReportPlay(ctx context.Context, id string, playedMs, totalMs int64) error {
	q := url.Values{
		"id":   {id},
		"time": {strconv.FormatInt(playedMs/1000, 10)},
	}
	if totalMs > 0 {
		q.Set("total", strconv.FormatInt(totalMs/1000, 10))
	}
	return p.write(ctx, "/scrobble/v1", q)
}

// Like 将曲目加入凭据账号的喜欢列表。
func (p *Provider) Like(ctx context.Context, id string) error {
	return p.write(ctx, "/like", url.Values{
		"id":   {id},
		"like": {"true"},
	})
}

// LikeCheck 查询曲目是否已在凭据账号的喜欢列表中。
// /song/like/check 直接按曲目 ID 回读，无需额外拉取完整喜欢列表。
func (p *Provider) LikeCheck(ctx context.Context, id string) (bool, error) {
	cookie := p.cookie.Load().(string)
	if cookie == "" {
		return false, fmt.Errorf("ncm api /song/like/check: credential not configured")
	}
	var resp struct {
		Code int     `json:"code"`
		IDs  []int64 `json:"ids"`
	}
	if err := p.getWithClient(ctx, p.writeClient, "/song/like/check",
		url.Values{"ids": {"[" + id + "]"}}, cookie, &resp); err != nil {
		return false, err
	}
	if resp.Code != http.StatusOK {
		return false, fmt.Errorf("ncm api /song/like/check: code %d", resp.Code)
	}
	for _, likedID := range resp.IDs {
		if strconv.FormatInt(likedID, 10) == id {
			return true, nil
		}
	}
	return false, nil
}

// AccountPlaylists 列出凭据账号的歌单摘要。
func (p *Provider) AccountPlaylists(ctx context.Context) ([]provider.AccountPlaylist, error) {
	cookie := p.cookie.Load().(string)
	if cookie == "" {
		return nil, fmt.Errorf("ncm api /user/playlist: credential not configured")
	}
	uid, err := p.credentialAccountUID(ctx)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code     int `json:"code"`
		Playlist []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			CoverURL   string `json:"coverImgUrl"`
			TrackCount int    `json:"trackCount"`
		} `json:"playlist"`
	}
	if err := p.getWithClient(ctx, p.writeClient, "/user/playlist",
		url.Values{"uid": {uid}}, cookie, &resp); err != nil {
		return nil, err
	}
	if resp.Code != http.StatusOK {
		return nil, fmt.Errorf("ncm api /user/playlist: code %d", resp.Code)
	}
	// 网易云把“我喜欢的音乐”放在首项；保持接口原顺序，不做特殊处理。
	out := make([]provider.AccountPlaylist, 0, len(resp.Playlist))
	for _, playlist := range resp.Playlist {
		out = append(out, provider.AccountPlaylist{
			ID:         strconv.FormatInt(playlist.ID, 10),
			Name:       playlist.Name,
			CoverURL:   playlist.CoverURL,
			TrackCount: playlist.TrackCount,
		})
	}
	return out, nil
}

func (p *Provider) credentialAccountUID(ctx context.Context) (string, error) {
	if p.st == nil {
		return "", fmt.Errorf("ncm credential account unavailable: re-login required")
	}
	owner, ok, err := p.st.GetCredentialOwner(ctx, p.ID())
	if err != nil {
		return "", fmt.Errorf("get ncm credential account: %w", err)
	}
	uid := strings.TrimSpace(owner.Account.UID)
	if !ok || uid == "" || uid == "0" {
		return "", fmt.Errorf("ncm credential account uid unavailable: re-login required")
	}
	return uid, nil
}

// AddToPlaylist 将曲目加入凭据账号的指定歌单。
func (p *Provider) AddToPlaylist(ctx context.Context, playlistID, trackID string) error {
	return p.write(ctx, "/playlist/tracks", url.Values{
		"op":        {"add"},
		"pid":       {playlistID},
		"tracks":    {trackID},
		"timestamp": {strconv.FormatInt(time.Now().UnixMilli(), 10)},
	})
}

// ---------- 二维码登录 ----------

// QR 登录状态码（NCM API 返回的 code）。
const (
	QRStatusExpired = 800 // 二维码过期
	QRStatusWaiting = 801 // 等待扫码
	QRStatusScanned = 802 // 已扫码待确认
	QRStatusOK      = 803 // 授权成功
)

// QRLoginStart 生成二维码：返回 key（轮询用）与 qrurl（渲染二维码的内容）。
func (p *Provider) QRLoginStart(ctx context.Context) (key, qrurl string, err error) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	var keyResp struct {
		Data struct {
			UniKey string `json:"unikey"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/login/qr/key", url.Values{"timestamp": {ts}}, "", &keyResp); err != nil {
		return "", "", err
	}
	if keyResp.Data.UniKey == "" {
		return "", "", fmt.Errorf("qr key: empty unikey")
	}

	var createResp struct {
		Data struct {
			QRURL string `json:"qrurl"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/login/qr/create",
		url.Values{"key": {keyResp.Data.UniKey}, "timestamp": {ts}}, "", &createResp); err != nil {
		return "", "", err
	}
	return keyResp.Data.UniKey, createResp.Data.QRURL, nil
}

// QRLoginPoll 轮询扫码状态。status 为 expired|waiting|scanned|ok；
// ok 时凭据已被提取、校验并热生效。
func (p *Provider) QRLoginPoll(ctx context.Context, key string) (string, string, error) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Cookie  string `json:"cookie"`
	}
	if err := p.get(ctx, "/login/qr/check",
		url.Values{"key": {key}, "timestamp": {ts}, "noCookie": {"true"}}, "", &resp); err != nil {
		return "", "", err
	}
	switch resp.Code {
	case QRStatusExpired:
		return "expired", resp.Message, nil
	case QRStatusWaiting:
		return "waiting", resp.Message, nil
	case QRStatusScanned:
		return "scanned", resp.Message, nil
	case QRStatusOK:
		musicU := extractCookieValue(resp.Cookie, "MUSIC_U")
		if musicU == "" {
			return "", "", fmt.Errorf("qr login succeeded but MUSIC_U missing in cookie")
		}
		if err := p.SetCredential(ctx, "MUSIC_U="+musicU); err != nil {
			return "", "", err
		}
		return "ok", "登录成功，凭据已生效", nil
	default:
		return "", "", fmt.Errorf("qr check: unexpected code %d (%s)", resp.Code, resp.Message)
	}
}

// extractCookieValue 从 Set-Cookie 串中提取指定 key 的值。
func extractCookieValue(cookieStr, key string) string {
	for part := range strings.SplitSeq(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, key+"="); ok {
			return v
		}
	}
	return ""
}

// ---------- Provider 接口 ----------
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

func (p *Provider) Search(ctx context.Context, query string, limit, offset int) ([]provider.Track, error) {
	limit, offset = normalizeSearchPage(limit, offset)
	q := url.Values{
		"keywords": {query},
		"type":     {"1"},
		"limit":    {strconv.Itoa(limit)},
		"offset":   {strconv.Itoa(offset)},
	}
	var resp struct {
		Code   int `json:"code"`
		Result struct {
			Songs []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Duration int64  `json:"duration"`
				Al       ncmAl  `json:"al"`
				Artists  []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"songs"`
		} `json:"result"`
	}
	if err := p.get(ctx, "/search", q, "", &resp); err != nil {
		return nil, err
	}
	out := make([]provider.Track, 0, len(resp.Result.Songs))
	for _, s := range resp.Result.Songs {
		out = append(out, provider.Track{
			Ref:          provider.NewRef(p.ID(), strconv.FormatInt(s.ID, 10)),
			Title:        s.Name,
			Artist:       joinArtists(namesOf(s.Artists)),
			DurationMs:   s.Duration,
			Album:        s.Al.Name,
			CoverURL:     s.Al.PicURL,
			SourceURL:    sourceURL(s.ID),
			Contributors: artistContributors(s.Artists),
		})
	}
	return out, nil
}

func (p *Provider) GetTrack(ctx context.Context, ref provider.TrackRef) (provider.Track, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.Track{}, err
	}
	q := url.Values{"ids": {id}}
	var resp struct {
		Code  int `json:"code"`
		Songs []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Dt   int64  `json:"dt"`
			Al   struct {
				Name   string `json:"name"`
				PicURL string `json:"picUrl"`
			} `json:"al"`
			Ar []struct {
				Name string `json:"name"`
			} `json:"ar"`
		} `json:"songs"`
	}
	if err := p.get(ctx, "/song/detail", q, "", &resp); err != nil {
		return provider.Track{}, err
	}
	if len(resp.Songs) == 0 {
		return provider.Track{}, fmt.Errorf("track not found: %s", ref)
	}
	s := resp.Songs[0]
	return provider.Track{
		Ref:          ref,
		Title:        s.Name,
		Artist:       joinArtists(namesOf(s.Ar)),
		DurationMs:   s.Dt,
		Album:        s.Al.Name,
		CoverURL:     s.Al.PicURL,
		SourceURL:    sourceURL(s.ID),
		Contributors: artistContributors(s.Ar),
	}, nil
}

func (p *Provider) Resolve(ctx context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.StreamLocator{}, err
	}
	q := url.Values{"id": {id}, "level": {p.level}}
	var resp struct {
		Code int `json:"code"`
		Data []struct {
			URL  string `json:"url"`
			Type string `json:"type"`
			Br   int64  `json:"br"`
			Size int64  `json:"size"`
			Expi int64  `json:"expi"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/song/url/v1", q, p.cookie.Load().(string), &resp); err != nil {
		return provider.StreamLocator{}, err
	}
	if len(resp.Data) == 0 || resp.Data[0].URL == "" {
		return provider.StreamLocator{}, fmt.Errorf(
			"no playable url for %s (可能需要登录或会员，请配置 ncm 凭据)", ref)
	}
	d := resp.Data[0]
	loc := provider.StreamLocator{
		URL:         d.URL,
		Format:      strings.ToLower(d.Type),
		SizeBytes:   d.Size,
		BitrateKbps: int(d.Br / 1000), // br 单位 bps
	}
	if d.Expi > 0 {
		loc.ExpiresAt = time.Now().Add(time.Duration(d.Expi) * time.Second)
	}
	return loc, nil
}

// ---------- 内部 ----------

// get 发 GET 请求并解码 JSON。cookie 非空时按文档以 query 参数传递。
func (p *Provider) get(ctx context.Context, path string, q url.Values, cookie string, out any) error {
	return p.getWithClient(ctx, p.client, path, q, cookie, out)
}

// write 使用短超时客户端执行账号写操作，并校验业务状态码。
func (p *Provider) write(ctx context.Context, path string, q url.Values) error {
	cookie := p.cookie.Load().(string)
	if cookie == "" {
		return fmt.Errorf("ncm api %s: credential not configured", path)
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := p.getWithClient(ctx, p.writeClient, path, q, cookie, &resp); err != nil {
		return err
	}
	if resp.Code != http.StatusOK {
		message := resp.Message
		if message == "" {
			message = resp.Msg
		}
		return fmt.Errorf("ncm api %s: code %d (%s)", path, resp.Code, message)
	}
	return nil
}

func (p *Provider) getWithClient(
	ctx context.Context,
	client *http.Client,
	path string,
	q url.Values,
	cookie string,
	out any,
) error {
	if cookie != "" {
		q.Set("cookie", cookie)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ncm api %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ncm api %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func namesOf(as []struct {
	Name string `json:"name"`
}) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

func joinArtists(names []string) string { return strings.Join(names, "/") }

// ---------- 富元数据辅助 ----------

// ncmAl 专辑信息（各曲目接口共用的 al 对象）。
type ncmAl struct {
	Name   string `json:"name"`
	PicURL string `json:"picUrl"`
}

const defaultCoverParam = "300y300"

// CoverMode 实现 provider.CoverModeAware：网易图床无防盗链，客户端可直连，
// 统一封面端点 302 到源站 URL，省服务器带宽。
func (*Provider) CoverMode() provider.CoverMode { return provider.CoverModeRedirect }

// ThumbnailCoverURL asks NetEase's image CDN for the default thumbnail while
// preserving any other source query parameters. Dropping param restores the
// original image URL.
func (*Provider) ThumbnailCoverURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("param", defaultCoverParam)
	u.RawQuery = q.Encode()
	return u.String()
}

func sourceURL(id int64) string {
	return "https://music.163.com/song?id=" + strconv.FormatInt(id, 10)
}

func artistContributors(as []struct {
	Name string `json:"name"`
}) []provider.Contributor {
	names := namesOf(as)
	out := make([]provider.Contributor, 0, len(names))
	for _, n := range names {
		out = append(out, provider.Contributor{Role: "artist", Name: n})
	}
	return out
}

// Lyrics 实现 provider.LyricsProvider：/lyric → lrc 原文 + 翻译。
func (p *Provider) Lyrics(ctx context.Context, ref provider.TrackRef) (provider.Lyrics, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.Lyrics{}, err
	}
	var resp struct {
		Code int `json:"code"`
		Lrc  struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
		Tlyric struct {
			Lyric string `json:"lyric"`
		} `json:"tlyric"`
	}
	if err := p.get(ctx, "/lyric", url.Values{"id": {id}}, p.cookie.Load().(string), &resp); err != nil {
		return provider.Lyrics{}, err
	}
	if resp.Lrc.Lyric == "" {
		return provider.Lyrics{}, fmt.Errorf("no lyrics for %s", ref)
	}
	return provider.Lyrics{Type: "lrc", LRC: resp.Lrc.Lyric, TLRC: resp.Tlyric.Lyric}, nil
}
