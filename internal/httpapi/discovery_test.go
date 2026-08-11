package httpapi

import (
	"context"
	"encoding/json"
	"errors"
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

// discoveryFakeProvider 同时实现 ArtistDetailer 与 RecommendationProvider。
type discoveryFakeProvider struct {
	id          string
	detail      provider.ArtistDetail
	detailErr   error
	detailCalls *[]string
	// detailName 非空时仅在该名字匹配时返回 detail（模拟真实 detailer 的名字敏感）
	detailName  string
	shelves     []provider.RecommendationShelf
	shelvesErr  error
}

func (p *discoveryFakeProvider) ID() string { return p.id }
func (*discoveryFakeProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}
func (*discoveryFakeProvider) GetTrack(context.Context, provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, errors.New("unused")
}
func (*discoveryFakeProvider) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, errors.New("unused")
}
func (p *discoveryFakeProvider) ArtistDetail(_ context.Context, name string) (provider.ArtistDetail, error) {
	if p.detailCalls != nil {
		*p.detailCalls = append(*p.detailCalls, p.id+":"+name)
	}
	if p.detailName != "" && name != p.detailName {
		return provider.ArtistDetail{}, errors.New("artist not found")
	}
	return p.detail, p.detailErr
}
func (p *discoveryFakeProvider) Recommendations(context.Context) ([]provider.RecommendationShelf, error) {
	return p.shelves, p.shelvesErr
}

type discoveryFixture struct {
	handler http.Handler
	st      *store.Store
	reg     *provider.Registry
	authm   *auth.Manager
	token   string
}

func setupDiscoveryEndpoints(t *testing.T) discoveryFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "discovery.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authm := auth.NewManager("", st)
	token := authm.IssueSession(auth.Identity{
		ID: "alice", Name: "Alice", Kind: "guest", Roles: []string{auth.RoleRequester},
	})
	if token == "" {
		t.Fatal("issue requester session")
	}
	reg := provider.NewRegistry()
	controls := control.NewService(nil, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, reg: reg, controls: controls,
		ws: wsapi.NewServer(authm, auth.NewPlayerRegistry(st), controls, st),
	}
	s.SetCoverSecret([]byte("test-secret"))
	return discoveryFixture{handler: s.Handler(), st: st, reg: reg, authm: authm, token: token}
}

func discoveryRequest(t *testing.T, f discoveryFixture, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestArtistProfileEndpoint(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm",
		detail: provider.ArtistDetail{
			Name: "周杰伦", EntityID: "777", AvatarURL: "https://img/avatar.jpg", Bio: "歌手",
		},
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("周杰伦"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist map[string]any `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist["name"] != "周杰伦" || body.Artist["provider"] != "ncm" || body.Artist["entity_id"] != "777" || body.Artist["bio"] != "歌手" {
		t.Fatalf("artist = %#v, want resolved ncm entity", body.Artist)
	}
	avatar, _ := body.Artist["avatar_url"].(string)
	if !strings.HasPrefix(avatar, "/api/v1/cover/ext/") {
		t.Fatalf("avatar_url = %q, want ext proxy", avatar)
	}
	for _, removed := range []string{"play_count", "last_played_at", "top_tracks"} {
		if _, exists := body.Artist[removed]; exists {
			t.Fatalf("artist unexpectedly contains removed field %q: %#v", removed, body.Artist)
		}
	}
}

func TestArtistProfileMultiArtistSegmentFallback(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm", detailCalls: &calls,
		detailName: "张晶",
		detail:     provider.ArtistDetail{Name: "张晶", EntityID: "777", Bio: "歌手"},
	})

	// 完整 join 串与前面各段都解析失败，第三段 "张晶" 成功 → 返回该段实体。
	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("塞壬唱片-MSR/F.L.O.A.T (白羽）/张晶"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "ncm" || body.Artist.EntityID != "777" || body.Artist.Bio != "歌手" {
		t.Fatalf("artist = %#v, want segment-resolved ncm entity", body.Artist)
	}
	// 调用序列：完整串 → 各段，取首个成功
	want := []string{"ncm:塞壬唱片-MSR/F.L.O.A.T (白羽）/张晶", "ncm:塞壬唱片-MSR", "ncm:F.L.O.A.T (白羽）", "ncm:张晶"}
	if len(calls) != len(want) {
		t.Fatalf("detail calls = %#v, want %#v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("detail call[%d] = %q, want %q (all = %#v)", i, calls[i], want[i], calls)
		}
	}
}

func TestArtistProfileAllProvidersFailReturnsNameOnly(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	f.reg.Register(&discoveryFakeProvider{id: "ncm", detailErr: errors.New("artist not found")})

	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("本地歌手"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist map[string]any `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Artist) != 1 || body.Artist["name"] != "本地歌手" {
		t.Fatalf("artist = %#v, want name only", body.Artist)
	}
}

func TestArtistProfileTrackRefProviderTakesPriority(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	var calls []string
	f.reg.Register(&discoveryFakeProvider{
		id: "aaa", detailCalls: &calls,
		detail: provider.ArtistDetail{Name: "全局首项", EntityID: "global"},
	})
	f.reg.Register(&discoveryFakeProvider{
		id: "zzz", detailCalls: &calls,
		detail: provider.ArtistDetail{Name: "锚定项", EntityID: "anchored"},
	})

	path := "/api/v1/artists/" + url.PathEscape("歌手") + "?track_ref=" + url.QueryEscape("zzz:track")
	rec := discoveryRequest(t, f, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist artistProfileResponse `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Provider != "zzz" || body.Artist.EntityID != "anchored" {
		t.Fatalf("artist = %#v, want anchored zzz entity", body.Artist)
	}
	if len(calls) != 1 || calls[0] != "zzz:歌手" {
		t.Fatalf("detail calls = %#v, want anchored provider only", calls)
	}
}

func TestArtistProfileValidation(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	rec := discoveryRequest(t, f, "/api/v1/artists/")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = discoveryRequest(t, f, "/api/v1/artists/x?track_ref=invalid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid track_ref status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "bad_request" {
		t.Fatalf("invalid track_ref error = %#v, want bad_request", errBody.Error)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/x", nil)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous artist status = %d, want 401", rec.Code)
	}

	listenerToken := f.authm.IssueSession(auth.Identity{
		ID: "bob", Name: "Bob", Kind: "guest", Roles: []string{auth.RoleListener},
	})
	req = httptest.NewRequest(http.MethodGet, "/api/v1/artists/x", nil)
	req.Header.Set("Authorization", "Bearer "+listenerToken)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("listener artist status = %d, want 403", rec.Code)
	}
}

func TestRecommendationsEndpoint(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm",
		shelves: []provider.RecommendationShelf{
			{
				ID: "toplist:1", Title: "飙升榜",
				Tracks: []provider.Track{{
					Ref: provider.NewRef("ncm", "9"), Title: "Top Song",
					Artist: "Artist", CoverURL: "https://cover/raw.jpg",
				}},
			},
			{
				ID: "toplist:2", Title: "新歌榜",
				Tracks: []provider.Track{{
					Ref: provider.NewRef("ncm", "10"), Title: "New Song",
					CoverURL: "https://cover/raw-2.jpg",
				}},
			},
		},
	})
	f.reg.Register(&discoveryFakeProvider{id: "zzz", shelvesErr: errors.New("sidecar down")})

	rec := discoveryRequest(t, f, "/api/v1/recommendations")
	if rec.Code != http.StatusOK {
		t.Fatalf("recommendations status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Shelves []provider.RecommendationShelf `json:"shelves"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Shelves) != 2 {
		t.Fatalf("shelves = %#v, want 2 (zzz 失败被跳过)", body.Shelves)
	}
	if body.Shelves[0].ID != "toplist:1" || body.Shelves[0].Title != "飙升榜" {
		t.Fatalf("shelf 0 = %#v", body.Shelves[0])
	}
	if got := body.Shelves[0].Tracks[0].CoverURL; got != "/api/v1/cover/ncm:9" {
		t.Fatalf("shelf cover = %q, want proxy path", got)
	}
}

// TestRecommendationsEmptyWithoutSource 无任何 provider 实现 → 200 空 shelves。
func TestRecommendationsEmptyWithoutSource(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	rec := discoveryRequest(t, f, "/api/v1/recommendations")
	if rec.Code != http.StatusOK {
		t.Fatalf("recommendations status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// wire 契约：空 shelves 必须是 [] 而非 null（客户端 shelves.map 直接可用）。
	if !strings.Contains(rec.Body.String(), `"shelves":[]`) {
		t.Fatalf("empty shelves body = %s, want \"shelves\":[]", rec.Body.String())
	}
	var body struct {
		Shelves []provider.RecommendationShelf `json:"shelves"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Shelves) != 0 {
		t.Fatalf("shelves = %#v, want empty", body.Shelves)
	}
}
