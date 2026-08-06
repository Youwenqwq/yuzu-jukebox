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
	results, err := p.SearchCategory(context.Background(), provider.SearchCategoryArtist, "周杰伦")
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

func TestEntityTracksPagination(t *testing.T) {
	t.Run("three page hard cap", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			if r.URL.Path != "/space/videos" || r.URL.Query().Get("mid") != "12345" || r.URL.Query().Get("ps") != "30" {
				t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(videoListResponse{Results: makeVideos(fmt.Sprintf("p%d-", pn), 30, -1)})
		}))
		defer server.Close()

		tracks, err := testProvider(server, "cookie").EntityTracks(context.Background(), provider.SearchCategoryArtist, "12345")
		if err != nil {
			t.Fatalf("EntityTracks: %v", err)
		}
		if requests != 3 || len(tracks) != 90 {
			t.Fatalf("requests = %d, tracks = %d; want 3 and 90", requests, len(tracks))
		}
		assertVideoTrack(t, tracks[0], "p1-0")
		assertVideoTrack(t, tracks[89], "p3-29")
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

		tracks, err := testProvider(server, "cookie").EntityTracks(context.Background(), provider.SearchCategoryArtist, "8")
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

		name, tracks, err := testProvider(server, "SESSDATA=favorite").ImportPlaylist(context.Background(), "2468")
		if err != nil {
			t.Fatalf("ImportPlaylist: %v", err)
		}
		if name != "" {
			t.Errorf("name = %q, want empty sidecar title", name)
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

	t.Run("caps at five hundred resources", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			_ = json.NewEncoder(w).Encode(videoListResponse{
				Results: makeVideos(fmt.Sprintf("p%d-", pn), 20, -1),
				Total:   999,
			})
		}))
		defer server.Close()

		_, tracks, err := testProvider(server, "cookie").ImportPlaylist(context.Background(), "1")
		if err != nil {
			t.Fatalf("ImportPlaylist: %v", err)
		}
		if requests != 25 || len(tracks) != 500 {
			t.Fatalf("requests = %d, tracks = %d; want 25 and 500", requests, len(tracks))
		}
	})

	t.Run("empty cookie fails before request", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, _, err := testProvider(server, "").ImportPlaylist(context.Background(), "42")
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
			Cover:      "//i0.hdslb.com/" + id + ".jpg",
			DurationMs: int64(i + 1),
			Published:  1_700_000_000,
		}
	}
	return videos
}

func assertVideoTrack(t *testing.T, track provider.Track, id string) {
	t.Helper()
	if track.Ref != provider.NewRef("bili", id) || track.Title != "title "+id || track.Artist != "uploader" || track.CoverURL != "https://i0.hdslb.com/"+id+".jpg" || track.SourceURL != "https://www.bilibili.com/video/"+id || len(track.Contributors) != 1 || track.Contributors[0].Role != "uploader" || track.Contributors[0].Name != "uploader" {
		t.Errorf("track = %+v", track)
	}
}
