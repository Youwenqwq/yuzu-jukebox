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
	base   string
	level  string
	st     *store.Store
	client *http.Client

	cookie atomic.Value // string，空串 = 未配置
}

func New(baseURL, level string, st *store.Store) *Provider {
	if level == "" {
		level = "exhigh"
	}
	p := &Provider{
		base:   strings.TrimRight(baseURL, "/"),
		level:  level,
		st:     st,
		client: &http.Client{Timeout: 15 * time.Second},
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
	if err := p.checkLogin(ctx, payload); err != nil {
		_ = p.st.UpsertCredential(ctx, p.ID(), payload, "invalid")
		return fmt.Errorf("credential validation failed: %w", err)
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
	if err := p.checkLogin(ctx, payload); err != nil {
		return "invalid"
	}
	return "ok"
}

// checkLogin 用 /login/status 验证 cookie 有效性（account 非空即已登录）。
func (p *Provider) checkLogin(ctx context.Context, cookie string) error {
	var resp struct {
		Data struct {
			Account *struct {
				ID int64 `json:"id"`
			} `json:"account"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/login/status", url.Values{}, cookie, &resp); err != nil {
		return err
	}
	if resp.Data.Account == nil {
		return fmt.Errorf("not logged in")
	}
	return nil
}

// ---------- Provider 接口 ----------

func (p *Provider) Search(ctx context.Context, query string) ([]provider.Track, error) {
	q := url.Values{"keywords": {query}, "type": {"1"}, "limit": {"30"}}
	var resp struct {
		Code   int `json:"code"`
		Result struct {
			Songs []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Duration int64  `json:"duration"`
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
			Ref:        provider.NewRef(p.ID(), strconv.FormatInt(s.ID, 10)),
			Title:      s.Name,
			Artist:     joinArtists(namesOf(s.Artists)),
			DurationMs: s.Duration,
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
			Ar   []struct {
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
		Ref:        ref,
		Title:      s.Name,
		Artist:     joinArtists(namesOf(s.Ar)),
		DurationMs: s.Dt,
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
		URL:    d.URL,
		Format: strings.ToLower(d.Type),
	}
	if d.Expi > 0 {
		loc.ExpiresAt = time.Now().Add(time.Duration(d.Expi) * time.Second)
	}
	return loc, nil
}

// ---------- 内部 ----------

// get 发 GET 请求并解码 JSON。cookie 非空时按文档以 query 参数传递。
func (p *Provider) get(ctx context.Context, path string, q url.Values, cookie string, out any) error {
	if cookie != "" {
		q.Set("cookie", cookie)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
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
