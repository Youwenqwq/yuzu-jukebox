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
	upstream  string // 假图床基地址
	lastRefer string // 上游最近一次收到的 Referer
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
	token := s.coverSigner.Mint("fake", fp.upstream+"/artist.jpg")
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
