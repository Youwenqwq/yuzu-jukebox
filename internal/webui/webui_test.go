package webui

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

func testFS() fs.FS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><div id=root></div>")},
		"favicon.svg":   {Data: []byte("<svg/>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
}

func newTestServer(t *testing.T, web config.WebConfig, adminPassword bool) *httptest.Server {
	t.Helper()
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "1")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("backend"))
	})
	srv := httptest.NewServer(newHandler(testFS(), backend, web, adminPassword))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, string(body)
}

func TestServesIndexAndAssets(t *testing.T) {
	srv := newTestServer(t, config.WebConfig{}, false)

	resp, body := get(t, srv.URL+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id=root`) {
		t.Fatalf("GET / = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("index Cache-Control = %q", got)
	}

	resp, body = get(t, srv.URL+"/assets/app.js")
	if resp.StatusCode != http.StatusOK || body != "console.log(1)" {
		t.Fatalf("GET /assets/app.js = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset Cache-Control = %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("asset Content-Type = %q", ct)
	}
}

// SPA fallback 只吃无扩展名路径；缺失的静态资源必须 404，
// 否则前端会拿到 HTML 当脚本解析。
func TestFallbackAndMissingAssets(t *testing.T) {
	srv := newTestServer(t, config.WebConfig{}, false)

	resp, body := get(t, srv.URL+"/room/lobby")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "id=root") {
		t.Fatalf("SPA fallback = %d %q", resp.StatusCode, body)
	}

	resp, _ = get(t, srv.URL+"/assets/missing.js")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", resp.StatusCode)
	}
}

// 后端独占的路径段永远直达后端：未知 API 不能被兜成 200 HTML。
func TestReservedSegmentsReachBackend(t *testing.T) {
	srv := newTestServer(t, config.WebConfig{}, false)

	for _, path := range []string{"/api", "/api/v1/typo", "/api/v1/rooms/x", "/stream/v1/ncm:1", "/ws/v1"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusTeapot || body != "backend" {
			t.Fatalf("%s = %d %q, want backend", path, resp.StatusCode, body)
		}
	}

	// 按路径段比较：/apix、/wsfoo 不是保留前缀，仍走静态托管。
	resp, body := get(t, srv.URL+"/apix")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "id=root") {
		t.Fatalf("/apix = %d %q, want SPA index", resp.StatusCode, body)
	}
}

func TestConfigJSReflectsServerConfig(t *testing.T) {
	web := config.WebConfig{
		Title:        "客厅点歌台",
		Favicon:      "/brand.svg",
		Accent:       "#6A8FD8",
		Scheme:       "light",
		OIDCClientID: "web-client",
	}
	srv := newTestServer(t, web, true)

	resp, body := get(t, srv.URL+"/config.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /config.js = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("config.js Cache-Control = %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("config.js Content-Type = %q", ct)
	}

	const prefix = "globalThis.YUZU_CONFIG = "
	idx := strings.Index(body, prefix)
	if idx < 0 {
		t.Fatalf("config.js missing assignment: %q", body)
	}
	raw := strings.TrimSuffix(strings.TrimSpace(body[idx+len(prefix):]), ";")
	var got runtimeConfig
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("config.js payload not JSON: %v (%q)", err, raw)
	}
	want := runtimeConfig{
		Server:               "",
		OIDCClientID:         "web-client",
		Title:                "客厅点歌台",
		Favicon:              "/brand.svg",
		Accent:               "#6A8FD8",
		Scheme:               "light",
		AdminPasswordEnabled: true,
	}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

// admin_password 为空时前端必须隐藏口令框：留空即关闭 elevate。
func TestConfigJSAdminPasswordDisabledWhenServerPasswordEmpty(t *testing.T) {
	srv := newTestServer(t, config.WebConfig{}, false)
	_, body := get(t, srv.URL+"/config.js")
	if strings.Contains(body, `"admin_password_enabled":true`) {
		t.Fatalf("admin_password_enabled should be false: %s", body)
	}
}

// 非 GET 请求不参与静态托管，直接交给后端。
func TestNonGETGoesToBackend(t *testing.T) {
	srv := newTestServer(t, config.WebConfig{}, false)
	resp, err := http.Post(srv.URL+"/anything", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("POST /anything = %d, want backend", resp.StatusCode)
	}
}
