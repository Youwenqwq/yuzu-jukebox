// Package webui 以内嵌静态资源的方式提供 WebUI：同源托管 SPA、
// 运行期生成 config.js，并把 API/流/WS 路径交回后端 mux。
//
// 前端产物在构建期注入（见 yuzu-deploy/server.Dockerfile 的
// --build-context web=…）；源码仓库只提交 dist/index.html 占位，
// 保证没有前端产物时 `go build` / `go test` 仍然可用。
package webui

import (
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

//go:embed all:dist
var embedded embed.FS

var distFS = mustSub(embedded, "dist")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("webui: " + err.Error())
	}
	return sub
}

// reserved 是后端独占的路径段：永不进入静态兜底，
// 避免未知 API 被 SPA fallback 成 200 text/html。
var reserved = []string{"/api", "/stream", "/ws"}

// isReserved：命中 path 本身或其后跟 / 的子路径。
func isReserved(p string) bool {
	for _, seg := range reserved {
		if p == seg || strings.HasPrefix(p, seg+"/") {
			return true
		}
	}
	return false
}

const configJSPath = "/config.js"

// Wrap 用嵌入的 dist 提供 WebUI，next 为后端 handler。
// adminPasswordEnabled 由服务端 admin_password 是否非空推导，
// 决定 config.js 是否让前端展示管理员口令输入框。
func Wrap(next http.Handler, web config.WebConfig, adminPasswordEnabled bool) http.Handler {
	return newHandler(distFS, next, web, adminPasswordEnabled)
}

func newHandler(fsys fs.FS, next http.Handler, web config.WebConfig, adminPasswordEnabled bool) http.Handler {
	return &handler{fsys: fsys, next: next, configJS: renderConfigJS(web, adminPasswordEnabled)}
}

type handler struct {
	fsys     fs.FS
	next     http.Handler
	configJS []byte
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isReserved(r.URL.Path) {
		h.next.ServeHTTP(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.next.ServeHTTP(w, r)
		return
	}

	if r.URL.Path == configJSPath {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		// 运行期配置：必须每次取最新，且不允许中间代理缓存。
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(h.configJS)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.HasSuffix(name, "/") {
		name += "index.html"
	}
	name = path.Clean(name)
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}

	if h.serveFile(w, r, name) {
		return
	}

	// SPA fallback：无扩展名的未知路径交给前端路由。
	// 带扩展名的缺失资源保持 404，不把缺失资源当页面返回。
	if strings.Contains(path.Base(name), ".") {
		http.NotFound(w, r)
		return
	}
	if !h.serveFile(w, r, "index.html") {
		http.NotFound(w, r)
	}
}

// serveFile 输出 fsys 中的 name；命中返回 true。
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := h.fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// 非 seeker 的 fs 实现（测试替身等）：整读后交给 ServeContent。
		data, rerr := fs.ReadFile(h.fsys, name)
		if rerr != nil {
			return false
		}
		rs = bytes.NewReader(data)
	}

	w.Header().Set("Cache-Control", cacheControl(name))
	// ServeContent 负责 Content-Type 推断、HEAD 与 Range。
	http.ServeContent(w, r, name, st.ModTime(), rs)
	return true
}

// cacheControl：带内容 hash 的 assets 长期 immutable；入口 HTML 每次校验；
// 其余静态文件短缓存。
func cacheControl(name string) string {
	switch {
	case name == "index.html":
		return "no-cache"
	case strings.HasPrefix(name, "assets/"):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=3600"
	}
}

// runtimeConfig 的字段名与 yuzu-jukebox-web 的 src/config.ts 严格对齐。
// server 恒为空串：内嵌模式下页面与 API 同源。
type runtimeConfig struct {
	Server               string `json:"server"`
	OIDCClientID         string `json:"oidc_client_id"`
	Title                string `json:"title"`
	Favicon              string `json:"favicon"`
	Accent               string `json:"accent"`
	Scheme               string `json:"scheme"`
	AdminPasswordEnabled bool   `json:"admin_password_enabled"`
}

func renderConfigJS(web config.WebConfig, adminPasswordEnabled bool) []byte {
	body, err := json.Marshal(runtimeConfig{
		Server:               "",
		OIDCClientID:         web.OIDCClientID,
		Title:                web.Title,
		Favicon:              web.Favicon,
		Accent:               web.Accent,
		Scheme:               web.Scheme,
		AdminPasswordEnabled: adminPasswordEnabled,
	})
	if err != nil {
		// 全字符串/布尔结构体，Marshal 不会失败。
		panic("webui: marshal runtime config: " + err.Error())
	}
	out := make([]byte, 0, len(body)+112)
	out = append(out, "/* 由 yuzu-server 生成：改本文件无效，请编辑 config.json 的 web 段。 */\n"...)
	out = append(out, "globalThis.YUZU_CONFIG = "...)
	out = append(out, body...)
	out = append(out, ";\n"...)
	return out
}
