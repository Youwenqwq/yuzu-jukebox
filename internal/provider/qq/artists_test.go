package qq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// TestArtistDetail 艺人名 → 歌手搜索首条 → /singer/{mid}/desc 富化。
func TestArtistDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/search_by_type":
			q := r.URL.Query()
			if got := q.Get("keyword"); got != "周杰伦" {
				t.Errorf("keyword = %q, want 周杰伦", got)
			}
			if got := q.Get("search_type"); got != "1" {
				t.Errorf("search_type = %q, want 1 (singer)", got)
			}
			_, _ = w.Write([]byte(envelope(map[string]any{"singer": []any{
				map[string]any{"mid": "004Z8Ihr0JIu5s", "name": "周杰伦", "subtitle": "华语歌手"},
			}})))
		case "/singer/004Z8Ihr0JIu5s/desc":
			_, _ = w.Write([]byte(envelope(map[string]any{"singer_list": []any{
				map[string]any{
					"basic_info": map[string]any{"name": "周杰伦"},
					"ex_info":    map[string]any{"desc": " 歌手、词曲创作人 "},
					"pic":        map[string]any{"pic": "https://img/avatar.jpg"},
				},
			}})))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	p := testProvider(t, server)

	got, err := p.ArtistDetail(context.Background(), "周杰伦")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	want := provider.ArtistDetail{
		Name: "周杰伦", AvatarURL: "https://img/avatar.jpg", Bio: "歌手、词曲创作人",
	}
	if got != want {
		t.Fatalf("ArtistDetail() = %#v, want %#v", got, want)
	}
}

// TestArtistDetailNotFound 名字在 QQ 侧不存在时返回错误（httpapi 降级为本地统计）。
func TestArtistDetailNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/search_by_type" {
			t.Errorf("path = %q, want search", r.URL.Path)
		}
		_, _ = w.Write([]byte(envelope(map[string]any{"singer": []any{}})))
	}))
	p := testProvider(t, server)

	if _, err := p.ArtistDetail(context.Background(), "nobody"); err == nil {
		t.Fatal("ArtistDetail = nil error, want not-found error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ArtistDetail error = %v, want not-found mention", err)
	}
}
