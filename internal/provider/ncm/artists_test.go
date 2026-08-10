package ncm

import (
	"context"
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
	want := provider.ArtistDetail{Name: "周杰伦", AvatarURL: "https://img/avatar.jpg", Bio: "歌手"}
	if got != want {
		t.Fatalf("ArtistDetail() = %#v, want %#v", got, want)
	}
}

// TestArtistDetailNotFound 名字在 NCM 侧不存在时返回错误（httpapi 降级为本地统计）。
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
