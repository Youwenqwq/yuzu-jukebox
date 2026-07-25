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
	"strings"
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

func (p *Provider) Search(ctx context.Context, query string) ([]provider.Track, error) {
	q := url.Values{"keywords": {query}, "limit": {"30"}}
	var resp struct {
		Results []struct {
			Bvid       string `json:"bvid"`
			Title      string `json:"title"`
			Author     string `json:"author"`
			Cover      string `json:"cover"`     // 可能是协议相对 URL（//i0.hdslb.com/...）
			Partition  string `json:"partition"` // 视频分区，作为 Album
			DurationMs int64  `json:"duration_ms"`
		} `json:"results"`
	}
	if err := p.get(ctx, "/search", q, p.cookie.Load().(string), &resp); err != nil {
		return nil, err
	}
	out := make([]provider.Track, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Bvid == "" {
			continue
		}
		out = append(out, provider.Track{
			Ref:          provider.NewRef(p.ID(), r.Bvid),
			Title:        r.Title,
			Artist:       r.Author,
			Album:        r.Partition,
			CoverURL:     normalizeCoverURL(r.Cover),
			SourceURL:    videoURL(r.Bvid),
			DurationMs:   r.DurationMs,
			Contributors: uploaderContributor(r.Author),
		})
	}
	return out, nil
}

func (p *Provider) GetTrack(ctx context.Context, ref provider.TrackRef) (provider.Track, error) {
	_, id, err := ref.Split()
	if err != nil {
		return provider.Track{}, err
	}
	var resp struct {
		Title  string `json:"title"`
		Author string `json:"author"`
		Cover  string `json:"cover"` // sidecar 现有字段；完整 https URL
		Pic    string `json:"pic"`   // 预留：上游 bilibili API 原生封面字段
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
	if err := p.get(ctx, "/detail", url.Values{"bvid": {id}}, "", &resp); err != nil {
		return provider.Track{}, err
	}
	// Resolve 固定播放第 1 P（page=1），时长必须与之一致：
	// 多分 P 视频的顶层 duration 是全集合计，直接用它会让
	// 房间的结束 timer 严重超调（流早完了，状态机还以为在放）。
	duration := resp.DurationMs
	if len(resp.Pages) > 0 && resp.Pages[0].DurationMs > 0 {
		duration = resp.Pages[0].DurationMs
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
	return provider.Track{
		Ref:          ref,
		Title:        resp.Title,
		Artist:       resp.Author,
		Album:        resp.TName,
		CoverURL:     normalizeCoverURL(cover),
		SourceURL:    videoURL(id),
		DurationMs:   duration,
		Contributors: uploaderContributor(uploader),
	}, nil
}

func (p *Provider) Resolve(ctx context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	_, id, err := ref.Split()
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
	if err := p.get(ctx, "/audio-url", url.Values{"bvid": {id}}, p.cookie.Load().(string), &resp); err != nil {
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
