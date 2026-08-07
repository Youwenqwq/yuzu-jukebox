package qq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

func TestSearchCategories(t *testing.T) {
	p := &Provider{}
	want := []provider.SearchCategory{
		provider.SearchCategorySong,
		provider.SearchCategoryArtist,
		provider.SearchCategoryAlbum,
		provider.SearchCategoryPlaylist,
	}
	if got := p.SearchCategories(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SearchCategories() = %#v, want %#v", got, want)
	}
}

func TestSearchCategoryUsesType(t *testing.T) {
	tests := []struct {
		category provider.SearchCategory
		wantType string
	}{
		{category: provider.SearchCategoryArtist, wantType: "1"},
		{category: provider.SearchCategoryAlbum, wantType: "2"},
		{category: provider.SearchCategoryPlaylist, wantType: "3"},
	}
	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/search/search_by_type" {
					t.Errorf("path = %q", r.URL.Path)
				}
				q := r.URL.Query()
				if got := q.Get("search_type"); got != tt.wantType {
					t.Errorf("search_type = %q, want %q", got, tt.wantType)
				}
				if got := q.Get("num"); got != "30" {
					t.Errorf("num = %q, want 30", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(envelope(map[string]any{})))
			}))
			defer server.Close()
			p := testProvider(t, server)
			if _, err := p.SearchCategory(context.Background(), tt.category, "query", 0, 0); err != nil {
				t.Fatalf("SearchCategory() error = %v", err)
			}
		})
	}
}

func TestSearchCategorySongWrapsSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelope(map[string]any{"song": []any{
			songFixture(testMid, testName, 101, 267, "周杰伦"),
		}})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	want, err := p.Search(context.Background(), "same", 0, 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	got, err := p.SearchCategory(context.Background(), provider.SearchCategorySong, "same", 0, 0)
	if err != nil {
		t.Fatalf("SearchCategory(song) error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("SearchCategory(song) = %d results, want %d", len(got), len(want))
	}
	if got[0].Type != provider.SearchCategorySong || got[0].Track == nil ||
		!reflect.DeepEqual(*got[0].Track, want[0]) {
		t.Fatalf("result = %#v, want wrapped search track", got[0])
	}
}

func TestSearchCategoryEntityMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("search_type") {
		case "1":
			_, _ = w.Write([]byte(envelope(map[string]any{"singer": []any{
				map[string]any{"id": 201, "mid": "singer-mid-1", "name": "歌手", "pic": "https://cover/singer", "song_num": 5, "album_num": 2, "mv_num": 1, "subtitle": "歌手简介"},
			}})))
		case "2":
			_, _ = w.Write([]byte(envelope(map[string]any{"album": []any{
				map[string]any{"id": 202, "mid": "album-mid-1", "name": "专辑", "pic": "https://cover/album", "time_public": "2020-01-01",
					"singer": "<em>歌手</em>", "singer_list": []any{map[string]any{"mid": "singer-mid-1", "name": "歌手"}}},
			}})))
		case "3":
			_, _ = w.Write([]byte(envelope(map[string]any{"songlist": []any{
				map[string]any{"id": 203, "title": "歌单", "picurl": "https://cover/playlist", "songnum": 123},
			}})))
		default:
			t.Errorf("unexpected search_type %q", r.URL.Query().Get("search_type"))
			_, _ = w.Write([]byte(envelope(map[string]any{})))
		}
	}))
	defer server.Close()
	p := testProvider(t, server)

	tests := []struct {
		category provider.SearchCategory
		want     provider.SearchResult
	}{
		{
			category: provider.SearchCategoryArtist,
			want: provider.SearchResult{
				Type: provider.SearchCategoryArtist, EntityID: "singer-mid-1",
				Name: "歌手", Detail: "歌手简介", CoverURL: "https://cover/singer",
			},
		},
		{
			category: provider.SearchCategoryAlbum,
			want: provider.SearchResult{
				Type: provider.SearchCategoryAlbum, EntityID: "album-mid-1",
				Name: "专辑", Detail: "歌手", CoverURL: "https://cover/album",
			},
		},
		{
			category: provider.SearchCategoryPlaylist,
			want: provider.SearchResult{
				Type: provider.SearchCategoryPlaylist, EntityID: "203",
				Name: "歌单", Detail: "123 首", CoverURL: "https://cover/playlist",
			},
		},
	}
	for _, tt := range tests {
		got, err := p.SearchCategory(context.Background(), tt.category, "query", 0, 0)
		if err != nil {
			t.Fatalf("%s: SearchCategory() error = %v", tt.category, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %d results, want 1", tt.category, len(got))
		}
		if !reflect.DeepEqual(got[0], tt.want) {
			t.Fatalf("%s: got %#v, want %#v", tt.category, got[0], tt.want)
		}
	}
}

func TestEntityTracksArtistPagedAndAlbumLocalSlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/singer/singer-mid/songs":
			if got := r.URL.Query().Get("num"); got != "30" {
				t.Errorf("songs num = %q, want 30", got)
			}
			if got := r.URL.Query().Get("page"); got != "2" {
				t.Errorf("songs page = %q, want 2", got)
			}
			var songs []any
			for i := 0; i < 30; i++ {
				songs = append(songs, songFixture("s-mid"+string(rune('a'+i)), "s", 1000+int64(i), 100, "a"))
			}
			_, _ = w.Write([]byte(envelope(map[string]any{"singer_mid": "singer-mid", "total_num": 80, "song_list": songs})))
		case "/album/album-mid/songs":
			var songs []any
			for i := 0; i < 10; i++ {
				songs = append(songs, songFixture("a-mid"+string(rune('a'+i)), "a", 2000+int64(i), 200, "a"))
			}
			_, _ = w.Write([]byte(envelope(map[string]any{"album_mid": "album-mid", "total_num": 10, "song_list": songs})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := testProvider(t, server)

	// 歌手：上游分页，offset=35 丢弃 5 个
	artistTracks, err := p.EntityTracks(context.Background(), provider.SearchCategoryArtist, "singer-mid", 30, 35)
	if err != nil {
		t.Fatalf("EntityTracks(artist) error = %v", err)
	}
	if len(artistTracks) != 25 {
		t.Fatalf("EntityTracks(artist) = %d tracks, want 25", len(artistTracks))
	}
	if artistTracks[0].Ref.String() != "qq:s-midf" {
		t.Fatalf("first artist track = %s, want qq:s-midf", artistTracks[0].Ref)
	}

	// 专辑：上游全量，本地切片 [3:8]
	albumTracks, err := p.EntityTracks(context.Background(), provider.SearchCategoryAlbum, "album-mid", 5, 3)
	if err != nil {
		t.Fatalf("EntityTracks(album) error = %v", err)
	}
	if len(albumTracks) != 5 {
		t.Fatalf("EntityTracks(album) = %d tracks, want 5", len(albumTracks))
	}
	if albumTracks[0].Ref.String() != "qq:a-midd" {
		t.Fatalf("first album track = %s, want qq:a-midd", albumTracks[0].Ref)
	}
}

func TestEntityTracksUnsupportedCategory(t *testing.T) {
	p := &Provider{}
	if _, err := p.EntityTracks(context.Background(), provider.SearchCategoryPlaylist, "1", 10, 0); err == nil {
		t.Fatal("EntityTracks(playlist) error = nil, want ErrNotSupported")
	}
}

func TestEntityAlbums(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/singer/singer-mid/albums" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("page"); got != "1" {
			t.Errorf("page = %q, want 1", got)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"singer_mid": "singer-mid", "total": 2, "album_list": []any{
			map[string]any{"id": 1, "mid": "alb-mid-1", "name": "专辑一", "time_public": "2020", "total_num": 10, "singer_name": "歌手"},
			map[string]any{"id": 2, "mid": "alb-mid-2", "name": "专辑二", "time_public": "2021", "total_num": 5, "singer_name": "歌手"},
		}})))
	}))
	defer server.Close()
	p := testProvider(t, server)

	got, err := p.EntityAlbums(context.Background(), "singer-mid", 10, 0)
	if err != nil {
		t.Fatalf("EntityAlbums() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("EntityAlbums() = %d, want 2", len(got))
	}
	first := got[0]
	if first.Type != provider.SearchCategoryAlbum || first.EntityID != "alb-mid-1" ||
		first.Name != "专辑一" || first.Detail != "歌手" {
		t.Fatalf("first = %#v", first)
	}
	if first.CoverURL != "https://y.gtimg.cn/music/photo_new/T002R300x300M000alb-mid-1.jpg" {
		t.Fatalf("CoverURL = %q", first.CoverURL)
	}
}

func TestJsonFieldNamesMatchContract(t *testing.T) {
	// 钉死 sidecar 序列化字段名：任何重构不得悄悄改名。
	var raw map[string]json.RawMessage
	body, _ := json.Marshal(songFixture(testMid, testName, 1, 2, "a"))
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id", "mid", "name", "type", "interval", "singer", "album"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("fixture missing field %q", want)
		}
	}
}
