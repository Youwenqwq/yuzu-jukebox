package ncm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

func TestSearchCategoryUsesNCMSearchType(t *testing.T) {
	tests := []struct {
		category provider.SearchCategory
		wantType string
	}{
		{category: provider.SearchCategorySong, wantType: "1"},
		{category: provider.SearchCategoryArtist, wantType: "100"},
		{category: provider.SearchCategoryAlbum, wantType: "10"},
		{category: provider.SearchCategoryPlaylist, wantType: "1000"},
	}
	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/search" {
					t.Errorf("path = %q, want /search", r.URL.Path)
				}
				if got := r.URL.Query().Get("type"); got != tt.wantType {
					t.Errorf("type = %q, want %q", got, tt.wantType)
				}
				if got := r.URL.Query().Get("keywords"); got != "query" {
					t.Errorf("keywords = %q, want query", got)
				}
				if got := r.URL.Query().Get("limit"); got != "30" {
					t.Errorf("limit = %q, want 30", got)
				}
				if got := r.URL.Query().Get("cookie"); got != "" {
					t.Errorf("anonymous search cookie = %q, want empty", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":200,"result":{}}`))
			}))
			defer server.Close()

			p := categoryTestProvider(server)
			if _, err := p.SearchCategory(context.Background(), tt.category, "query"); err != nil {
				t.Fatalf("SearchCategory() error = %v", err)
			}
		})
	}
}

func TestSearchCategorySongWrapsSearch(t *testing.T) {
	const fixture = `{"code":200,"result":{"songs":[{"id":101,"name":"First","duration":1234,"al":{"name":"Record","picUrl":"https://cover/101"},"artists":[{"name":"A"},{"name":"B"}]},{"id":102,"name":"Second","duration":5678,"al":{"name":"Other","picUrl":"https://cover/102"},"artists":[{"name":"C"}]}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	want, err := p.Search(context.Background(), "same")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	got, err := p.SearchCategory(context.Background(), provider.SearchCategorySong, "same")
	if err != nil {
		t.Fatalf("SearchCategory(song) error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("SearchCategory(song) returned %d results, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Type != provider.SearchCategorySong || got[i].Track == nil {
			t.Fatalf("result %d = %#v, want song with track", i, got[i])
		}
		if !reflect.DeepEqual(*got[i].Track, want[i]) {
			t.Fatalf("result %d track = %#v, want %#v", i, *got[i].Track, want[i])
		}
	}
}

func TestSearchCategoryEntityMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("type") {
		case "100":
			_, _ = w.Write([]byte(`{"code":200,"result":{"artists":[{"id":201,"name":"Singer","picUrl":"","img1v1Url":"https://cover/artist","briefDesc":"artist detail"}]}}`))
		case "10":
			_, _ = w.Write([]byte(`{"code":200,"result":{"albums":[{"id":202,"name":"Album","picUrl":"https://cover/album","artist":{"name":"Singer"}}]}}`))
		case "1000":
			_, _ = w.Write([]byte(`{"code":200,"result":{"playlists":[{"id":203,"name":"Playlist","trackCount":123,"coverImgUrl":"https://cover/playlist"}]}}`))
		default:
			t.Errorf("unexpected type %q", r.URL.Query().Get("type"))
			_, _ = w.Write([]byte(`{"code":200,"result":{}}`))
		}
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	tests := []struct {
		category provider.SearchCategory
		want     provider.SearchResult
	}{
		{
			category: provider.SearchCategoryArtist,
			want: provider.SearchResult{
				Type: provider.SearchCategoryArtist, EntityID: "201", Name: "Singer",
				Detail: "artist detail", CoverURL: "https://cover/artist",
			},
		},
		{
			category: provider.SearchCategoryAlbum,
			want: provider.SearchResult{
				Type: provider.SearchCategoryAlbum, EntityID: "202", Name: "Album",
				Detail: "Singer", CoverURL: "https://cover/album",
			},
		},
		{
			category: provider.SearchCategoryPlaylist,
			want: provider.SearchResult{
				Type: provider.SearchCategoryPlaylist, EntityID: "203", Name: "Playlist",
				Detail: "123 首", CoverURL: "https://cover/playlist",
			},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			got, err := p.SearchCategory(context.Background(), tt.category, "entity")
			if err != nil {
				t.Fatalf("SearchCategory() error = %v", err)
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
				t.Fatalf("SearchCategory() = %#v, want [%#v]", got, tt.want)
			}
		})
	}
}

func TestEntityTracksArtistAndAlbum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cookie"); got != "" {
			t.Errorf("anonymous drill cookie = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artist/top/song":
			if got := r.URL.Query().Get("id"); got != "artist-id" {
				t.Errorf("artist id = %q, want artist-id", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"songs":[{"id":301,"name":"Artist Song","duration":3010,"al":{"name":"Artist Album","picUrl":"https://cover/301"},"artists":[{"name":"Lead"},{"name":"Guest"}]}]}`))
		case "/album":
			if got := r.URL.Query().Get("id"); got != "album-id" {
				t.Errorf("album id = %q, want album-id", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"songs":[{"id":302,"name":"Album Song","dt":3020,"al":{"name":"Drilled Album","picUrl":"https://cover/302"},"ar":[{"name":"Album Artist"}]}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	artistTracks, err := p.EntityTracks(context.Background(), provider.SearchCategoryArtist, "artist-id")
	if err != nil {
		t.Fatalf("EntityTracks(artist) error = %v", err)
	}
	wantArtist := provider.Track{
		Ref:        provider.NewRef("ncm", "301"),
		Title:      "Artist Song",
		Artist:     "Lead/Guest",
		DurationMs: 3010,
		Album:      "Artist Album",
		CoverURL:   "https://cover/301",
		SourceURL:  "https://music.163.com/song?id=301",
		Contributors: []provider.Contributor{
			{Role: "artist", Name: "Lead"},
			{Role: "artist", Name: "Guest"},
		},
	}
	if len(artistTracks) != 1 || !reflect.DeepEqual(artistTracks[0], wantArtist) {
		t.Fatalf("artist tracks = %#v, want [%#v]", artistTracks, wantArtist)
	}

	albumTracks, err := p.EntityTracks(context.Background(), provider.SearchCategoryAlbum, "album-id")
	if err != nil {
		t.Fatalf("EntityTracks(album) error = %v", err)
	}
	wantAlbum := provider.Track{
		Ref:          provider.NewRef("ncm", "302"),
		Title:        "Album Song",
		Artist:       "Album Artist",
		DurationMs:   3020,
		Album:        "Drilled Album",
		CoverURL:     "https://cover/302",
		SourceURL:    "https://music.163.com/song?id=302",
		Contributors: []provider.Contributor{{Role: "artist", Name: "Album Artist"}},
	}
	if len(albumTracks) != 1 || !reflect.DeepEqual(albumTracks[0], wantAlbum) {
		t.Fatalf("album tracks = %#v, want [%#v]", albumTracks, wantAlbum)
	}
}

func TestImportPlaylistPaginatesThousandSongPages(t *testing.T) {
	type songFixture struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}

	var detailRequests, trackRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/playlist/detail":
			detailRequests++
			if got := r.URL.Query().Get("id"); got != "9876" {
				t.Errorf("detail id = %q, want 9876", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"playlist": map[string]any{
					"name":        "Long Playlist",
					"coverImgUrl": "https://p1.music.126.net/playlist-cover.jpg",
				},
			})
		case "/playlist/track/all":
			trackRequests++
			if got := r.URL.Query().Get("id"); got != "9876" {
				t.Errorf("track id = %q, want 9876", got)
			}
			if got := r.URL.Query().Get("limit"); got != "1000" {
				t.Errorf("limit = %q, want 1000", got)
			}
			offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
			if err != nil {
				t.Errorf("invalid offset %q: %v", r.URL.Query().Get("offset"), err)
			}
			count := 0
			switch offset {
			case 0, 1000:
				count = 1000
			case 2000:
				count = 300
			default:
				t.Errorf("unexpected offset %d", offset)
			}
			songs := make([]songFixture, count)
			for i := range songs {
				id := int64(offset + i + 1)
				songs[i] = songFixture{ID: id, Name: "song-" + strconv.FormatInt(id, 10)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": songs})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	p := categoryTestProvider(server)
	p.cookie.Store("")
	name, coverURL, tracks, err := p.ImportPlaylist(context.Background(), "9876")
	if err != nil {
		t.Fatalf("ImportPlaylist() error = %v", err)
	}
	if name != "Long Playlist" {
		t.Errorf("name = %q, want Long Playlist", name)
	}
	if coverURL != "https://p1.music.126.net/playlist-cover.jpg" {
		t.Errorf("coverURL = %q, want detail coverImgUrl", coverURL)
	}
	if len(tracks) != 2300 {
		t.Fatalf("tracks = %d, want 2300", len(tracks))
	}
	for _, check := range []struct {
		index int
		ref   provider.TrackRef
	}{
		{index: 0, ref: provider.NewRef("ncm", "1")},
		{index: 999, ref: provider.NewRef("ncm", "1000")},
		{index: 1000, ref: provider.NewRef("ncm", "1001")},
		{index: 2299, ref: provider.NewRef("ncm", "2300")},
	} {
		if got := tracks[check.index].Ref; got != check.ref {
			t.Errorf("tracks[%d].Ref = %q, want %q", check.index, got, check.ref)
		}
	}
	if detailRequests != 1 || trackRequests != 3 {
		t.Fatalf("detail requests = %d, track-all requests = %d; want 1 and 3", detailRequests, trackRequests)
	}
}

func TestCategoryUnsupported(t *testing.T) {
	p := &Provider{}
	if _, err := p.SearchCategory(context.Background(), provider.SearchCategory("unknown"), "q"); !errors.Is(err, provider.ErrNotSupported) {
		t.Fatalf("SearchCategory(unknown) error = %v, want ErrNotSupported", err)
	}
	for _, category := range []provider.SearchCategory{provider.SearchCategoryPlaylist, provider.SearchCategorySong, "unknown"} {
		if _, err := p.EntityTracks(context.Background(), category, "id"); !errors.Is(err, provider.ErrNotSupported) {
			t.Fatalf("EntityTracks(%q) error = %v, want ErrNotSupported", category, err)
		}
	}
}

func categoryTestProvider(server *httptest.Server) *Provider {
	return &Provider{base: server.URL, client: server.Client()}
}
