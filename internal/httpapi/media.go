package httpapi

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// coverClient 封面代理专用：源站图床偶发慢响应，超时收紧。
var coverClient = &http.Client{Timeout: 10 * time.Second}

// cover 统一封面代理。客户端拿到的 cover_url 一律是 /api/v1/cover/{track_ref}：
//   - local provider：服务上传时提取的内嵌封面文件
//   - 其他 provider：GetTrack 取源站 URL，带 CoverAware 头转发
//     （B 站图床无 Referer 会 403，与音频流同坑同解）
func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	ref := provider.TrackRef(r.PathValue("ref"))
	p, id, err := s.reg.ForRef(ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// local：内嵌封面文件直接出图
	if id != "" && p.ID() == "local" {
		mf, err := s.st.GetMediaFile(r.Context(), id)
		if err != nil || mf.CoverPath == "" {
			writeErr(w, http.StatusNotFound, "not_found", "no cover")
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=2592000")
		http.ServeFile(w, r, mf.CoverPath)
		return
	}

	track, err := p.GetTrack(r.Context(), ref)
	if err != nil || track.CoverURL == "" {
		writeErr(w, http.StatusNotFound, "not_found", "no cover")
		return
	}
	if s.ncmCoverDirect {
		if _, ok := p.(provider.CoverAware); !ok {
			http.Redirect(w, r, track.CoverURL, http.StatusFound)
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", track.CoverURL, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	if ca, ok := p.(provider.CoverAware); ok {
		for k, vs := range ca.CoverHeaders() {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	resp, err := coverClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "provider_error", "cover fetch: "+resp.Status)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && strings.HasPrefix(ct, "image/") {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, io.LimitReader(resp.Body, 20<<20))
}

// lyrics 歌词端点。LyricsProvider 是可选接口，provider 未实现时 501。
func (s *Server) lyrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleListener); !ok {
		return
	}
	ref := provider.TrackRef(r.URL.Query().Get("track_ref"))
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "track_ref required")
		return
	}
	p, _, err := s.reg.ForRef(ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	lp, ok := p.(provider.LyricsProvider)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "not_supported", "provider has no lyrics capability")
		return
	}
	l, err := lp.Lyrics(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, l)
}
