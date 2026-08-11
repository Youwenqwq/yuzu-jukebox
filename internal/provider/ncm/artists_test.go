package ncm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// TestArtistDetail 艺人名 → type=100 搜索取首条 → /artist/detail 富化。
func TestArtistDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cookie"); got != "" {
			t.Errorf("anonymous artist detail cookie = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			if got := r.URL.Query().Get("keywords"); got != "周杰伦" {
				t.Errorf("search keywords = %q, want 周杰伦", got)
			}
			if got := r.URL.Query().Get("type"); got != "100" {
				t.Errorf("search type = %q, want 100 (artist)", got)
			}
			if got := r.URL.Query().Get("limit"); got != "1" {
				t.Errorf("search limit = %q, want 1", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"result":{"artists":[{"id":777,"name":"周杰伦"}]}}`))
		case "/artist/detail":
			if got := r.URL.Query().Get("id"); got != "777" {
				t.Errorf("artist id = %q, want 777", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"artist":{"id":777,"name":"周杰伦","picUrl":"https://img/avatar.jpg","briefDesc":"歌手"}}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	got, err := p.ArtistDetail(context.Background(), "周杰伦")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	want := provider.ArtistDetail{Name: "周杰伦", EntityID: "777", AvatarURL: "https://img/avatar.jpg", Bio: "歌手"}
	if got != want {
		t.Fatalf("ArtistDetail() = %#v, want %#v", got, want)
	}
}

func TestArtistDetailByIDUsesDescriptionFallbackWithoutSearch(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.URL.Query().Get("id"); got != "777" {
			t.Errorf("artist id = %q, want 777", got)
		}
		switch r.URL.Path {
		case "/artist/detail":
			_, _ = w.Write([]byte(`{"code":200,"data":{"artist":{"name":"周杰伦","cover":"https://img/cover.jpg","briefDesc":""}}}`))
		case "/artist/desc":
			_, _ = w.Write([]byte(`{"code":200,"introduction":[{"ti":"艺人介绍","txt":"华语歌手"},{"ti":"","txt":"词曲创作人"},{"ti":" ","txt":" "}]} `))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	got, err := categoryTestProvider(server).ArtistDetailByID(context.Background(), "777")
	if err != nil {
		t.Fatalf("ArtistDetailByID error = %v", err)
	}
	want := provider.ArtistDetail{
		Name: "周杰伦", EntityID: "777", AvatarURL: "https://img/cover.jpg",
		Bio: "艺人介绍\n华语歌手\n\n词曲创作人",
	}
	if got != want {
		t.Fatalf("ArtistDetailByID() = %#v, want %#v", got, want)
	}
	wantPaths := []string{"/artist/detail", "/artist/desc"}
	if len(paths) != len(wantPaths) {
		t.Fatalf("paths = %#v, want %#v (no search)", paths, wantPaths)
	}
	for i := range wantPaths {
		if paths[i] != wantPaths[i] {
			t.Fatalf("path[%d] = %q, want %q", i, paths[i], wantPaths[i])
		}
	}
}

func TestArtistDetailAvatarFallbacks(t *testing.T) {
	tests := []struct {
		name         string
		searchFields string
		detailFields string
		wantAvatar   string
	}{
		{
			name:         "detail cover",
			searchFields: `,"picUrl":"https://img/search.jpg"`,
			detailFields: `,"cover":"https://img/detail-cover.jpg"`,
			wantAvatar:   "https://img/detail-cover.jpg",
		},
		{
			name:         "search pic",
			searchFields: `,"picUrl":"https://img/search.jpg"`,
			wantAvatar:   "https://img/search.jpg",
		},
		{
			name:         "search square pic",
			searchFields: `,"img1v1Url":"https://img/search-square.jpg"`,
			wantAvatar:   "https://img/search-square.jpg",
		},
		{
			name: "no upstream avatar",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/search":
					_, _ = w.Write([]byte(`{"code":200,"result":{"artists":[{"id":32540734,"name":"塞壬唱片-MSR"` + tc.searchFields + `}]}}`))
				case "/artist/detail":
					_, _ = w.Write([]byte(`{"code":200,"data":{"artist":{"id":32540734,"name":"塞壬唱片-MSR","briefDesc":"音乐厂牌"` + tc.detailFields + `}}}`))
				default:
					t.Errorf("unexpected path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			p := categoryTestProvider(server)
			got, err := p.ArtistDetail(context.Background(), "塞壬唱片-MSR")
			if err != nil {
				t.Fatalf("ArtistDetail error = %v", err)
			}
			want := provider.ArtistDetail{
				Name:      "塞壬唱片-MSR",
				EntityID:  "32540734",
				AvatarURL: tc.wantAvatar,
				Bio:       "音乐厂牌",
			}
			if got != want {
				t.Fatalf("ArtistDetail() = %#v, want %#v", got, want)
			}
			if tc.wantAvatar == "" {
				body, err := json.Marshal(got)
				if err != nil {
					t.Fatalf("Marshal ArtistDetail: %v", err)
				}
				if strings.Contains(string(body), `"avatar_url"`) {
					t.Fatalf("ArtistDetail JSON = %s, want avatar_url omitted", body)
				}
			}
		})
	}
}

// TestArtistDetailNotFound 名字在 NCM 侧不存在时返回错误。
func TestArtistDetailNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":200,"result":{"artists":[]}}`))
	}))
	defer server.Close()
	p := categoryTestProvider(server)

	if _, err := p.ArtistDetail(context.Background(), "nobody"); err == nil {
		t.Fatal("ArtistDetail = nil error, want not-found error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ArtistDetail error = %v, want not-found mention", err)
	}
}
