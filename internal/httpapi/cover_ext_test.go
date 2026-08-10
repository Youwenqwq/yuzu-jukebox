package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

// coverFakeProvider 带回源头（CoverAware）与分类检索的假 provider。
type coverFakeProvider struct {
	upstream  string     // 假图床基地址
	lastRefer string     // 上游最近一次收到的 Referer
	lastQuery url.Values // 上游最近一次收到的查询参数
}

func (p *coverFakeProvider) ID() string { return "fake" }
func (p *coverFakeProvider) Search(ctx context.Context, query string, limit, offset int) ([]provider.Track, error) {
	return []provider.Track{{
		Ref: provider.NewRef("fake", "t1"), Title: "歌", CoverURL: p.upstream + "/track.jpg",
	}}, nil
}
func (p *coverFakeProvider) GetTrack(ctx context.Context, ref provider.TrackRef) (provider.Track, error) {
	_, id, _ := ref.Split()
	return provider.Track{Ref: ref, Title: id, CoverURL: p.upstream + "/track.jpg"}, nil
}
func (p *coverFakeProvider) Resolve(ctx context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, provider.ErrNotSupported
}
func (p *coverFakeProvider) CoverHeaders() http.Header {
	return http.Header{"Referer": {"https://fake.example/"}}
}
func (p *coverFakeProvider) SearchCategories() []provider.SearchCategory {
	return []provider.SearchCategory{provider.SearchCategorySong, provider.SearchCategoryArtist}
}
func (p *coverFakeProvider) SearchCategory(ctx context.Context, cat provider.SearchCategory, query string, limit, offset int) ([]provider.SearchResult, error) {
	return []provider.SearchResult{{
		Type: provider.SearchCategoryArtist, EntityID: "42", Name: "某艺人",
		CoverURL: p.upstream + "/artist.jpg",
	}}, nil
}
func (p *coverFakeProvider) EntityTracks(ctx context.Context, cat provider.SearchCategory, entityID string, limit, offset int) ([]provider.Track, error) {
	return nil, provider.ErrNotSupported
}

type coverThumbnailProvider struct {
	*coverFakeProvider
}

func (*coverThumbnailProvider) ThumbnailCoverURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("param", "300y300")
	u.RawQuery = q.Encode()
	return u.String()
}

type coverRedirectProvider struct{}

func (*coverRedirectProvider) ID() string { return "redirect" }
func (*coverRedirectProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, provider.ErrNotSupported
}
func (*coverRedirectProvider) GetTrack(context.Context, provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, provider.ErrNotSupported
}
func (*coverRedirectProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, provider.ErrNotSupported
}
func (*coverRedirectProvider) ThumbnailCoverURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("param", "300y300")
	u.RawQuery = q.Encode()
	return u.String()
}

// coverModeRedirectProvider 显式声明 Redirect（未实现 CoverAware）。
type coverModeRedirectProvider struct{ coverRedirectProvider }

func (*coverModeRedirectProvider) CoverMode() provider.CoverMode { return provider.CoverModeRedirect }

// coverModeProxyProvider 显式声明 Proxy（未实现 CoverAware）。
type coverModeProxyProvider struct{ coverRedirectProvider }

func (*coverModeProxyProvider) CoverMode() provider.CoverMode { return provider.CoverModeProxy }

// coverAwareRedirectProvider 同时实现 CoverAware 与 Redirect 声明——
// 安全网要求恒代理（302 会丢掉 Referer）。
type coverAwareRedirectProvider struct{ coverFakeProvider }

func (*coverAwareRedirectProvider) CoverMode() provider.CoverMode { return provider.CoverModeRedirect }

func setupCoverServer(t *testing.T) (*Server, *coverFakeProvider) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cover.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fp := &coverFakeProvider{}
	// 假图床：记录 Referer，回一张最小 PNG
	fp.upstream = func() string { return "" }() // 占位，下方 httptest 启动后填充
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.lastRefer = r.Header.Get("Referer")
		fp.lastQuery = r.URL.Query()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	t.Cleanup(upstream.Close)
	fp.upstream = upstream.URL

	reg := provider.NewRegistry()
	reg.Register(fp)
	authm := auth.NewManager("", st)
	controls := control.NewService(nil, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, reg: reg, controls: controls,
		ws: wsapi.NewServer(authm, auth.NewPlayerRegistry(st), controls, st),
	}
	s.SetCoverSecret([]byte("0123456789abcdef0123456789abcdef"))
	return s, fp
}

func TestCoverExtProxiesEntityImageWithHeaders(t *testing.T) {
	s, fp := setupCoverServer(t)
	token := s.coverSigner.Mint("fake", fp.upstream+"/artist.jpg?token=abc")
	if token == "" {
		t.Fatal("Signer.Mint returned empty")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cover/ext/"+token, nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("coverExt status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
	if fp.lastRefer != "https://fake.example/" {
		t.Fatalf("upstream Referer = %q, want CoverAware header", fp.lastRefer)
	}
	if got := fp.lastQuery.Get("token"); got != "abc" || fp.lastQuery.Get("param") != "" {
		t.Fatalf("non-thumbnail provider query = %q, want token only", fp.lastQuery.Encode())
	}
}

func TestCoverExtAppliesThumbnailByDefault(t *testing.T) {
	s, fp := setupCoverServer(t)
	s.reg.Register(&coverThumbnailProvider{coverFakeProvider: fp})
	token := s.coverSigner.Mint("fake", fp.upstream+"/artist.jpg?token=abc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cover/ext/"+token, nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("coverExt status = %d, body %s", rec.Code, rec.Body)
	}
	if got := fp.lastQuery.Get("param"); got != "300y300" {
		t.Fatalf("thumbnail param = %q, want 300y300", got)
	}
	if got := fp.lastQuery.Get("token"); got != "abc" {
		t.Fatalf("source query token = %q, want preserved", got)
	}
}

func TestCoverExtOriginalSkipsThumbnail(t *testing.T) {
	s, fp := setupCoverServer(t)
	s.reg.Register(&coverThumbnailProvider{coverFakeProvider: fp})
	token := s.coverSigner.Mint("fake", fp.upstream+"/artist.jpg?token=abc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cover/ext/"+token+"?size=original", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("coverExt status = %d, body %s", rec.Code, rec.Body)
	}
	if got := fp.lastQuery.Get("param"); got != "" {
		t.Fatalf("thumbnail param = %q, want omitted", got)
	}
	if got := fp.lastQuery.Get("token"); got != "abc" {
		t.Fatalf("source query token = %q, want preserved", got)
	}
}

func TestProxyCoverDirectRedirectThumbnailSelection(t *testing.T) {
	// coverRedirectProvider 未声明 CoverMode，走全局默认（默认直连）。
	s := &Server{coverDirectDefault: true}
	p := &coverRedirectProvider{}
	tests := []struct {
		name    string
		target  string
		wantURL string
	}{
		{
			name:    "default thumbnail",
			target:  "/api/v1/cover/redirect%3At1",
			wantURL: "https://cover/image.jpg?param=300y300&token=abc",
		},
		{
			name:    "original",
			target:  "/api/v1/cover/redirect%3At1?size=original",
			wantURL: "https://cover/image.jpg?token=abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			s.proxyCover(rec, req, p, "https://cover/image.jpg?token=abc")
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			if got := rec.Header().Get("Location"); got != tt.wantURL {
				t.Fatalf("Location = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

// TestProxyCoverModePriority 封面取图模式决策优先级：
// 声明 > 全局默认；CoverAware 恒代理（安全网）。
func TestProxyCoverModePriority(t *testing.T) {
	var lastRefer string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastRefer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("fake-image"))
	}))
	defer upstream.Close()

	// 声明 Redirect + 全局默认代理 → 以声明为准：302
	rec := httptest.NewRecorder()
	s := &Server{}
	s.proxyCover(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cover/x", nil),
		&coverModeRedirectProvider{}, upstream.URL+"/r.jpg")
	if rec.Code != http.StatusFound {
		t.Fatalf("declared redirect + default proxy: status = %d, want 302", rec.Code)
	}

	// 声明 Proxy + 全局默认直连 → 以声明为准：代理取图
	rec = httptest.NewRecorder()
	s2 := &Server{coverDirectDefault: true}
	s2.proxyCover(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cover/x", nil),
		&coverModeProxyProvider{}, upstream.URL+"/p.jpg")
	if rec.Code != http.StatusOK || rec.Body.String() != "fake-image" {
		t.Fatalf("declared proxy + default redirect: status = %d body = %q", rec.Code, rec.Body.String())
	}

	// CoverAware + 声明 Redirect + 默认直连 → 恒代理，且注入 Referer
	rec = httptest.NewRecorder()
	s2.proxyCover(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cover/x", nil),
		&coverAwareRedirectProvider{}, upstream.URL+"/a.jpg")
	if rec.Code != http.StatusOK || rec.Body.String() != "fake-image" {
		t.Fatalf("cover-aware declared redirect: status = %d body = %q", rec.Code, rec.Body.String())
	}
	if lastRefer != "https://fake.example/" {
		t.Fatalf("CoverAware Referer = %q, want injected", lastRefer)
	}

	// 未声明 + 全局默认代理 → 代理取图
	rec = httptest.NewRecorder()
	s.proxyCover(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cover/x", nil),
		&coverRedirectProvider{}, upstream.URL+"/u.jpg")
	if rec.Code != http.StatusOK || rec.Body.String() != "fake-image" {
		t.Fatalf("undeclared + default proxy: status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestProxyCoverRejectsEmptyThumbnailURL(t *testing.T) {
	s := &Server{}
	p := &coverThumbnailProvider{coverFakeProvider: &coverFakeProvider{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cover/fake%3At1", nil)
	s.proxyCover(rec, req, p, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCoverExtRejectsForgedTokens(t *testing.T) {
	s, fp := setupCoverServer(t)
	valid := s.coverSigner.Mint("fake", fp.upstream+"/artist.jpg")

	// 防线 = 无法伪造：客户端没有密钥，构造不出指向任意目标的合法 token。
	cases := map[string]string{
		"garbage":  "not-a-token",
		"tampered": valid[:len(valid)-2] + "xx",
	}
	for name, token := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cover/ext/"+url.PathEscape(token), nil)
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestCoverExtUnknownProvider(t *testing.T) {
	s, _ := setupCoverServer(t)
	token := s.coverSigner.Mint("nosuch", "http://example.com/x.jpg")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cover/ext/"+token, nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSearchRewritesTrackAndEntityCovers(t *testing.T) {
	s, fp := setupCoverServer(t)
	authm := auth.NewManager("", s.st)
	token := authm.IssueSession(auth.Identity{
		ID: "alice", Name: "Alice", Kind: "guest", Roles: []string{auth.RoleRequester},
	})
	s.authm = authm

	// 曲目封面 → /api/v1/cover/{ref}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?provider=fake&q=x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.Handler().ServeHTTP(rec, req)
	var songResp struct {
		Tracks []provider.Track `json:"tracks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &songResp); err != nil || len(songResp.Tracks) != 1 {
		t.Fatalf("search response: %v %s", err, rec.Body)
	}
	want := "/api/v1/cover/" + url.PathEscape("fake:t1")
	if songResp.Tracks[0].CoverURL != want {
		t.Fatalf("track cover = %q, want %q", songResp.Tracks[0].CoverURL, want)
	}

	// 实体封面 → /api/v1/cover/ext/{token}，且该 URL 可直接取图（闭环）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/search?provider=fake&q=x&category=artist", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.Handler().ServeHTTP(rec, req)
	var entResp struct {
		Results []provider.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entResp); err != nil || len(entResp.Results) != 1 {
		t.Fatalf("category response: %v %s", err, rec.Body)
	}
	coverURL := entResp.Results[0].CoverURL
	if !strings.HasPrefix(coverURL, "/api/v1/cover/ext/") {
		t.Fatalf("entity cover = %q, want ext proxy path", coverURL)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, coverURL, nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("entity cover fetch = %d, body %s", rec.Code, rec.Body)
	}
	if fp.lastRefer != "https://fake.example/" {
		t.Fatalf("upstream Referer = %q", fp.lastRefer)
	}
}

func TestProxiedTrackCoverIdempotent(t *testing.T) {
	tr := provider.Track{Ref: "fake:t1", CoverURL: "/api/v1/cover/fake%3At1"}
	proxiedTrackCover(&tr)
	if tr.CoverURL != "/api/v1/cover/fake%3At1" {
		t.Fatalf("already-proxied cover rewritten: %q", tr.CoverURL)
	}
	tr = provider.Track{Ref: "fake:t2"}
	proxiedTrackCover(&tr)
	if tr.CoverURL != "" {
		t.Fatalf("empty cover rewritten: %q", tr.CoverURL)
	}
}
