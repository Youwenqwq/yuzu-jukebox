package bili

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// TestArtistDetailSnapshot 无凭据时用 /search/up 搜索快照的 face/sign 富化。
func TestArtistDetailSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/up" {
			t.Errorf("path = %q, want /search/up", r.URL.Path)
		}
		if got := r.URL.Query().Get("keywords"); got != "老番茄" {
			t.Errorf("keywords = %q, want 老番茄", got)
		}
		if got := r.Header.Get("X-Yuzu-Bilibili-Cookie"); got != "" {
			t.Errorf("anonymous search sent cookie %q", got)
		}
		_, _ = w.Write([]byte(`{"results":[{"mid":1577803,"name":"老番茄","face":"//i0.hdslb.com/bfs/face/up.jpg","sign":"知名游戏区UP主"}]}`))
	}))
	defer server.Close()
	p := testProvider(server, "")

	got, err := p.ArtistDetail(context.Background(), "老番茄")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	want := provider.ArtistDetail{
		Name:      "老番茄",
		EntityID:  "1577803",
		AvatarURL: "https://i0.hdslb.com/bfs/face/up.jpg", // 协议相对 URL 归一化
		Bio:       "知名游戏区UP主",
	}
	if got != want {
		t.Fatalf("ArtistDetail() = %#v, want %#v", got, want)
	}
}

// TestArtistDetailWithCredential 有凭据时升级为 /space/acc/info 权威档案。
func TestArtistDetailWithCredential(t *testing.T) {
	var sawAccInfo bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/up":
			_, _ = w.Write([]byte(`{"results":[{"mid":1577803,"name":"老番茄","face":"https://i0.hdslb.com/bfs/face/old.jpg","sign":"快照签名"}]}`))
		case "/space/acc/info":
			sawAccInfo = true
			if got := r.URL.Query().Get("mid"); got != "1577803" {
				t.Errorf("mid = %q, want 1577803", got)
			}
			if got := r.Header.Get("X-Yuzu-Bilibili-Cookie"); got != "SESSDATA=abc; bili_jct=csrf; DedeUserID=1" {
				t.Errorf("cookie = %q, want complete three-item cookie", got)
			}
			_, _ = w.Write([]byte(`{"mid":1577803,"name":"老番茄","face":"https://i0.hdslb.com/bfs/face/new.jpg","sign":"权威签名"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p := testProvider(server, "SESSDATA=abc; bili_jct=csrf; DedeUserID=1")

	got, err := p.ArtistDetail(context.Background(), "老番茄")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	if !sawAccInfo {
		t.Fatal("acc/info not called with credential set")
	}
	want := provider.ArtistDetail{
		Name: "老番茄", EntityID: "1577803", AvatarURL: "https://i0.hdslb.com/bfs/face/new.jpg", Bio: "权威签名",
	}
	if got != want {
		t.Fatalf("ArtistDetail() = %#v, want %#v", got, want)
	}
}

// TestArtistDetailAccInfoFailureDegrades 权威档案失败时保留搜索快照（不阻断）。
func TestArtistDetailAccInfoFailureDegrades(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/up":
			_, _ = w.Write([]byte(`{"results":[{"mid":1577803,"name":"老番茄","face":"https://i0.hdslb.com/bfs/face/up.jpg","sign":"快照签名"}]}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	p := testProvider(server, "SESSDATA=abc; bili_jct=csrf; DedeUserID=1")

	got, err := p.ArtistDetail(context.Background(), "老番茄")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	if got.Name != "老番茄" || got.EntityID != "1577803" || got.Bio != "快照签名" || got.AvatarURL != "https://i0.hdslb.com/bfs/face/up.jpg" {
		t.Fatalf("degraded detail = %#v, want search snapshot", got)
	}
}

func TestArtistDetailCookieGateKeepsSearchSnapshot(t *testing.T) {
	var sawAccInfo bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/up":
			_, _ = w.Write([]byte(`{"results":[{"mid":1577803,"name":"老番茄","face":"https://i0.hdslb.com/bfs/face/up.jpg","sign":"快照签名"}]}`))
		case "/space/acc/info":
			sawAccInfo = true
			t.Error("acc/info called with fewer than three cookie pairs")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p := testProvider(server, "SESSDATA=abc; bili_jct=csrf; not-a-pair;")

	got, err := p.ArtistDetail(context.Background(), "老番茄")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	if sawAccInfo {
		t.Fatal("acc/info called despite insufficient cookie items")
	}
	if got.Name != "老番茄" || got.EntityID != "1577803" || got.Bio != "快照签名" || got.AvatarURL != "https://i0.hdslb.com/bfs/face/up.jpg" {
		t.Fatalf("gated detail = %#v, want search snapshot", got)
	}
}

func TestArtistDetailVerifiesFirstSearchHitName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/up" {
			t.Errorf("path = %q, want /search/up only", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"results":[{"mid":1,"name":"另一个 UP"},{"mid":1577803,"name":"老番茄"}]}`))
	}))
	defer server.Close()
	p := testProvider(server, "")

	if _, err := p.ArtistDetail(context.Background(), "  老番茄  "); err == nil {
		t.Fatal("ArtistDetail = nil error, want first-hit name mismatch error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ArtistDetail error = %v, want not-found mention", err)
	}
}

func TestArtistDetailNameVerificationIgnoresCaseAndOuterSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"mid":42,"name":"  MixedCase UP  ","face":"//img/face.jpg"}]}`))
	}))
	defer server.Close()

	got, err := testProvider(server, "").ArtistDetail(context.Background(), "mixedcase up")
	if err != nil {
		t.Fatalf("ArtistDetail error = %v", err)
	}
	if got.EntityID != "42" {
		t.Fatalf("ArtistDetail = %#v, want case-insensitive trimmed match", got)
	}
}

// TestArtistDetailNotFound 名字不存在时返回错误。
func TestArtistDetailNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	p := testProvider(server, "")

	if _, err := p.ArtistDetail(context.Background(), "nobody"); err == nil {
		t.Fatal("ArtistDetail = nil error, want not-found error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ArtistDetail error = %v, want not-found mention", err)
	}
}
