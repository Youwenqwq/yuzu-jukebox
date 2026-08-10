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
	id         string
	detail     provider.ArtistDetail
	detailErr  error
	shelves    []provider.RecommendationShelf
	shelvesErr error
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
func (p *discoveryFakeProvider) ArtistDetail(context.Context, string) (provider.ArtistDetail, error) {
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
	ctx := context.Background()
	for _, row := range []struct {
		ref, title, artist string
		startedAt          int64
	}{
		{"ncm:1", "晴天", "周杰伦", 100},
		{"ncm:1", "晴天", "周杰伦", 200},
		{"ncm:1", "晴天", "周杰伦", 300},
		{"ncm:2", "稻香", "周杰伦", 400},
		{"ncm:3", "海阔天空", "Beyond", 500},
	} {
		if err := f.st.AddPlayHistory(ctx, "room-1", row.ref, row.title, row.artist, "alice", row.startedAt, row.startedAt+1, "finished"); err != nil {
			t.Fatal(err)
		}
	}
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm",
		detail: provider.ArtistDetail{
			Name: "周杰伦", AvatarURL: "https://img/avatar.jpg", Bio: "歌手",
		},
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("周杰伦"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist struct {
			Name         string                  `json:"name"`
			PlayCount    int                     `json:"play_count"`
			LastPlayedAt int64                   `json:"last_played_at"`
			AvatarURL    string                  `json:"avatar_url"`
			Bio          string                  `json:"bio"`
			TopTracks    []store.ArtistTrackStat `json:"top_tracks"`
		} `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	artist := body.Artist
	if artist.Name != "周杰伦" || artist.PlayCount != 4 || artist.LastPlayedAt != 400 {
		t.Fatalf("artist = %#v, want 4 plays last at 400", artist)
	}
	if artist.Bio != "歌手" || !strings.HasPrefix(artist.AvatarURL, "/api/v1/cover/ext/") {
		t.Fatalf("enrichment = bio %q avatar %q, want bio + ext proxy avatar", artist.Bio, artist.AvatarURL)
	}
	if len(artist.TopTracks) != 2 {
		t.Fatalf("top tracks = %#v, want 2", artist.TopTracks)
	}
	if artist.TopTracks[0].TrackRef != "ncm:1" || artist.TopTracks[0].PlayCount != 3 {
		t.Fatalf("top track 0 = %#v, want ncm:1 count 3", artist.TopTracks[0])
	}
	if artist.TopTracks[0].CoverURL != "/api/v1/cover/ncm:1" {
		t.Fatalf("top track cover = %q, want proxy path", artist.TopTracks[0].CoverURL)
	}
}

// TestArtistProfileDegradesWithoutProvider 富化失败（名字不存在/provider 不可用）
// 时降级为纯本地统计，不造假头像/简介。
func TestArtistProfileDegradesWithoutProvider(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	ctx := context.Background()
	if err := f.st.AddPlayHistory(ctx, "room-1", "local:1", "Local Song", "本地歌手", "alice", 100, 200, "finished"); err != nil {
		t.Fatal(err)
	}
	f.reg.Register(&discoveryFakeProvider{
		id: "ncm", detailErr: errors.New("artist not found"),
	})

	rec := discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("本地歌手"))
	if rec.Code != http.StatusOK {
		t.Fatalf("artist status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist struct {
			PlayCount int    `json:"play_count"`
			AvatarURL string `json:"avatar_url"`
			Bio       string `json:"bio"`
		} `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.PlayCount != 1 || body.Artist.AvatarURL != "" || body.Artist.Bio != "" {
		t.Fatalf("degraded artist = %#v, want stats only", body.Artist)
	}
}

func TestArtistProfileValidation(t *testing.T) {
	f := setupDiscoveryEndpoints(t)
	rec := discoveryRequest(t, f, "/api/v1/artists/")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 多艺人 join 串（含 /）经 {name...} 通配仍可匹配
	ctx := context.Background()
	if err := f.st.AddPlayHistory(ctx, "room-1", "ncm:9", "合唱", "A/B", "alice", 100, 200, "finished"); err != nil {
		t.Fatal(err)
	}
	rec = discoveryRequest(t, f, "/api/v1/artists/"+url.PathEscape("A/B"))
	if rec.Code != http.StatusOK {
		t.Fatalf("slashed name status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Artist struct {
			Name      string `json:"name"`
			PlayCount int    `json:"play_count"`
		} `json:"artist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Artist.Name != "A/B" || body.Artist.PlayCount != 1 {
		t.Fatalf("slashed artist = %#v, want A/B with 1 play", body.Artist)
	}

	// 未认证 → 401
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/x", nil)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous artist status = %d, want 401", rec.Code)
	}

	// 仅 listener 角色 → 403
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
