package bili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

func testProvider(server *httptest.Server, cookie string) *Provider {
	p := &Provider{
		base:   server.URL,
		client: server.Client(),
	}
	p.cookie.Store(cookie)
	return p
}

func TestThumbnailCoverURL(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain jpg",
			raw:  "https://i0.hdslb.com/bfs/archive/cover.jpg",
			want: "https://i0.hdslb.com/bfs/archive/cover.jpg@672w_378h_1c.webp",
		},
		{
			name: "already suffixed",
			raw:  "https://i1.hdslb.com/bfs/archive/cover.jpg@672w_378h_1c.webp",
			want: "https://i1.hdslb.com/bfs/archive/cover.jpg@672w_378h_1c.webp",
		},
		{
			name: "preserves query",
			raw:  "https://archive.biliimg.com/bfs/archive/cover.jpg?token=abc",
			want: "https://archive.biliimg.com/bfs/archive/cover.jpg@672w_378h_1c.webp?token=abc",
		},
		{name: "empty", raw: "", want: ""},
		{
			name: "unparseable",
			raw:  "https://i0.hdslb.com/%zz",
			want: "https://i0.hdslb.com/%zz",
		},
		{
			name: "foreign host",
			raw:  "https://not-hdslb.com/cover.jpg",
			want: "https://not-hdslb.com/cover.jpg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.ThumbnailCoverURL(tt.raw); got != tt.want {
				t.Fatalf("ThumbnailCoverURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestCoverMode 封面取图模式契约：B 站图床需 Referer，必须服务器代理。
func TestCoverMode(t *testing.T) {
	p := &Provider{}
	if got := p.CoverMode(); got != provider.CoverModeProxy {
		t.Fatalf("CoverMode() = %q, want %q", got, provider.CoverModeProxy)
	}
}

func TestSearchCategoryArtistMapping(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/search/up" {
			t.Errorf("path = %q, want /search/up", r.URL.Path)
		}
		if got := r.URL.Query().Get("keywords"); got != "周杰伦" {
			t.Errorf("keywords = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "30" {
			t.Errorf("limit = %q, want 30", got)
		}
		if got := r.URL.Query().Get("pn"); got != "1" {
			t.Errorf("pn = %q, want 1", got)
		}
		if got := r.Header.Get(cookieHeader); got != "SESSDATA=test" {
			t.Errorf("cookie header = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"mid": 12345, "name": "某UP", "face": "//i0.hdslb.com/face.jpg", "fans": 1000, "sign": "简介"},
				{"mid": 67890, "name": "无简介UP", "face": "https://i1.hdslb.com/face.jpg", "fans": 42, "sign": ""},
			},
		})
	}))
	defer server.Close()

	p := testProvider(server, "SESSDATA=test")
	if got := p.SearchCategories(); len(got) != 2 || got[0] != provider.SearchCategorySong || got[1] != provider.SearchCategoryArtist {
		t.Fatalf("SearchCategories() = %v", got)
	}
	results, err := p.SearchCategory(context.Background(), provider.SearchCategoryArtist, "周杰伦", 0, 0)
	if err != nil {
		t.Fatalf("SearchCategory: %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if got := results[0]; got.Type != provider.SearchCategoryArtist || got.EntityID != "12345" || got.Name != "某UP" || got.Detail != "简介" || got.CoverURL != "https://i0.hdslb.com/face.jpg" || got.Track != nil {
		t.Errorf("first result = %+v", got)
	}
	if got := results[1].Detail; got != "42 粉丝" {
		t.Errorf("empty-sign detail = %q, want fan fallback", got)
	}
}

func TestSearchCategoryArtistPaging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/up" {
			t.Errorf("path = %q, want /search/up", r.URL.Path)
		}
		if got := r.URL.Query().Get("keywords"); got != "分页 UP" {
			t.Errorf("keywords = %q, want 分页 UP", got)
		}
		if got := r.URL.Query().Get("limit"); got != "30" {
			t.Errorf("limit = %q, want 30", got)
		}
		if got := r.URL.Query().Get("pn"); got != "2" {
			t.Errorf("pn = %q, want 2", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer server.Close()

	if _, err := testProvider(server, "cookie").SearchCategory(
		context.Background(), provider.SearchCategoryArtist, "分页 UP", 30, 30,
	); err != nil {
		t.Fatalf("SearchCategory: %v", err)
	}
}

func TestSearchCategorySongPaging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		if got := r.URL.Query().Get("keywords"); got != "分页视频" {
			t.Errorf("keywords = %q, want 分页视频", got)
		}
		if got := r.URL.Query().Get("limit"); got != "12" {
			t.Errorf("limit = %q, want 12", got)
		}
		if got := r.URL.Query().Get("offset"); got != "7" {
			t.Errorf("offset = %q, want 7", got)
		}
		_ = json.NewEncoder(w).Encode(videoListResponse{
			Results: makeVideos("song-", 1, -1),
		})
	}))
	defer server.Close()

	results, err := testProvider(server, "cookie").SearchCategory(
		context.Background(), provider.SearchCategorySong, "分页视频", 12, 7,
	)
	if err != nil {
		t.Fatalf("SearchCategory: %v", err)
	}
	if len(results) != 1 || results[0].Type != provider.SearchCategorySong || results[0].Track == nil {
		t.Fatalf("results = %+v, want one song result", results)
	}
	assertVideoTrack(t, *results[0].Track, "song-0")
}

func TestEntityTracksPagination(t *testing.T) {
	t.Run("offset starts midway through upstream page", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			if r.URL.Path != "/space/videos" || r.URL.Query().Get("mid") != "12345" || r.URL.Query().Get("ps") != "30" {
				t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			wantPN := requests + 1
			if pn != wantPN {
				t.Errorf("request %d pn = %d, want %d", requests, pn, wantPN)
			}
			_ = json.NewEncoder(w).Encode(videoListResponse{Results: makeVideos(fmt.Sprintf("p%d-", pn), 30, -1)})
		}))
		defer server.Close()

		tracks, err := testProvider(server, "cookie").EntityTracks(
			context.Background(), provider.SearchCategoryArtist, "12345", 40, 35,
		)
		if err != nil {
			t.Fatalf("EntityTracks: %v", err)
		}
		if requests != 2 || len(tracks) != 40 {
			t.Fatalf("requests = %d, tracks = %d; want 2 and 40", requests, len(tracks))
		}
		assertVideoTrack(t, tracks[0], "p2-5")
		assertVideoTrack(t, tracks[39], "p3-14")
	})

	t.Run("short page stops early", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			count := 30
			if pn == 2 {
				count = 7
			}
			_ = json.NewEncoder(w).Encode(videoListResponse{Results: makeVideos(fmt.Sprintf("p%d-", pn), count, -1)})
		}))
		defer server.Close()

		tracks, err := testProvider(server, "cookie").EntityTracks(
			context.Background(), provider.SearchCategoryArtist, "8", 50, 0,
		)
		if err != nil {
			t.Fatalf("EntityTracks: %v", err)
		}
		if requests != 2 || len(tracks) != 37 {
			t.Fatalf("requests = %d, tracks = %d; want 2 and 37", requests, len(tracks))
		}
	})
}

func TestImportPlaylist(t *testing.T) {
	t.Run("paginates to total and skips unavailable videos", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.URL.Path != "/fav/resource/list" || r.URL.Query().Get("media_id") != "2468" || r.URL.Query().Get("ps") != "20" {
				t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			if got := r.Header.Get(cookieHeader); got != "SESSDATA=favorite" {
				t.Errorf("cookie header = %q", got)
			}
			pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			videos := makeVideos(fmt.Sprintf("p%d-", pn), 20, -1)
			if pn == 1 {
				videos[3].Bvid = ""
			}
			_ = json.NewEncoder(w).Encode(videoListResponse{Results: videos, Total: 25})
		}))
		defer server.Close()

		name, coverURL, tracks, err := testProvider(server, "SESSDATA=favorite").ImportPlaylist(context.Background(), "2468")
		if err != nil {
			t.Fatalf("ImportPlaylist: %v", err)
		}
		if name != "" {
			t.Errorf("name = %q, want empty sidecar title", name)
		}
		if coverURL != "" {
			t.Errorf("coverURL = %q, want empty until sidecar exposes folder cover", coverURL)
		}
		if requests != 2 {
			t.Fatalf("requests = %d, want 2", requests)
		}
		if len(tracks) != 24 {
			t.Fatalf("tracks = %d, want 24 (25 resources minus unavailable)", len(tracks))
		}
		assertVideoTrack(t, tracks[0], "p1-0")
		assertVideoTrack(t, tracks[len(tracks)-1], "p2-4")
	})

	t.Run("caps at one thousand resources", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			if pn > 50 {
				t.Errorf("requested page %d beyond 1000-resource cap", pn)
			}
			_ = json.NewEncoder(w).Encode(videoListResponse{
				Results: makeVideos(fmt.Sprintf("p%d-", pn), 20, -1),
				Total:   2000,
			})
		}))
		defer server.Close()

		_, coverURL, tracks, err := testProvider(server, "cookie").ImportPlaylist(context.Background(), "1")
		if err != nil {
			t.Fatalf("ImportPlaylist: %v", err)
		}
		if coverURL != "" {
			t.Errorf("coverURL = %q, want empty until sidecar exposes folder cover", coverURL)
		}
		if requests != 50 || len(tracks) != 1000 {
			t.Fatalf("requests = %d, tracks = %d; want 50 and 1000", requests, len(tracks))
		}
		assertVideoTrack(t, tracks[len(tracks)-1], "p50-19")
	})

	t.Run("empty cookie fails before request", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, _, _, err := testProvider(server, "").ImportPlaylist(context.Background(), "42")
		if err == nil {
			t.Fatal("ImportPlaylist unexpectedly succeeded")
		}
		if requests != 0 {
			t.Fatalf("requests = %d, want 0", requests)
		}
	})
}

func makeVideos(prefix string, count, emptyAt int) []videoResult {
	videos := make([]videoResult, count)
	for i := range videos {
		id := prefix + strconv.Itoa(i)
		if i == emptyAt {
			id = ""
		}
		videos[i] = videoResult{
			Bvid:       id,
			Title:      "title " + id,
			Author:     "uploader",
			Mid:        42,
			Cover:      "//i0.hdslb.com/" + id + ".jpg",
			DurationMs: int64(i + 1),
			Published:  1_700_000_000,
		}
	}
	return videos
}

func assertVideoTrack(t *testing.T, track provider.Track, id string) {
	t.Helper()
	if track.Ref != provider.NewRef("bili", id) || track.Title != "title "+id || track.Artist != "uploader" || track.CoverURL != "https://i0.hdslb.com/"+id+".jpg" || track.SourceURL != "https://www.bilibili.com/video/"+id || len(track.Contributors) != 1 || track.Contributors[0].Role != "uploader" || track.Contributors[0].Name != "uploader" || track.Contributors[0].EntityID != "42" {
		t.Errorf("track = %+v", track)
	}
}
