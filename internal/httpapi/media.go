package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
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
	s.proxyCover(w, r, p, track.CoverURL)
}

// coverExt 代理非曲目实体（艺人/专辑/歌单）的封面。
// 实体没有 TrackRef，封面 URL 由服务端在搜索结果序列化时签入
// 防伪造 token（HMAC，密钥派生自 secret_key）：
// 客户端只能回放服务端签发过的目标，无法指定任意 URL——
// 拒绝做成 url 透传式开放代理（SSRF 面）。
func (s *Server) coverExt(w http.ResponseWriter, r *http.Request) {
	pid, rawURL, ok := s.openCoverToken(r.PathValue("token"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid cover token")
		return
	}
	p, exists := s.reg.Get(pid)
	if !exists {
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown provider: "+pid)
		return
	}
	s.proxyCover(w, r, p, rawURL)
}

// proxyCover 带回源头转发一张封面图并写出响应。ncmCoverDirect 模式下，
// 无 CoverAware 的 provider（即无需 Referer 的源站）直接 302 到源站。
func (s *Server) proxyCover(w http.ResponseWriter, r *http.Request, p provider.Provider, rawURL string) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid cover url")
		return
	}
	if s.ncmCoverDirect {
		if _, ok := p.(provider.CoverAware); !ok {
			http.Redirect(w, r, rawURL, http.StatusFound)
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", rawURL, nil)
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

// ---------- 实体封面 token ----------

// coverTokenHMAC 派生实体封面 token 的 HMAC 密钥（域分离）。
func (s *Server) coverTokenHMAC(payload string) string {
	mac := hmac.New(sha256.New, s.coverSecret)
	mac.Write([]byte("cover-ext\n"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// mintCoverToken 签发实体封面 token；coverSecret 未配置时返回空串
// （调用方保持原始 URL，行为与旧版一致）。
func (s *Server) mintCoverToken(providerID, rawURL string) string {
	if len(s.coverSecret) == 0 || rawURL == "" {
		return ""
	}
	payload := providerID + "\n" + rawURL
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.coverTokenHMAC(payload)
}

// openCoverToken 校验并解出 token 内的 provider 与源站 URL。
func (s *Server) openCoverToken(token string) (providerID, rawURL string, ok bool) {
	if len(s.coverSecret) == 0 {
		return "", "", false
	}
	body, sig, found := strings.Cut(token, ".")
	if !found {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", false
	}
	payload := string(raw)
	if !hmac.Equal([]byte(sig), []byte(s.coverTokenHMAC(payload))) {
		return "", "", false
	}
	pid, u, found := strings.Cut(payload, "\n")
	if !found || pid == "" || u == "" {
		return "", "", false
	}
	return pid, u, true
}

// proxiedEntityCover 把实体封面改写为 /api/v1/cover/ext/{token} 代理路径；
// 无法签发（无密钥）或已是代理/相对路径时原样返回。
func (s *Server) proxiedEntityCover(providerID, rawURL string) string {
	if rawURL == "" || strings.HasPrefix(rawURL, "/") {
		return rawURL
	}
	if token := s.mintCoverToken(providerID, rawURL); token != "" {
		return "/api/v1/cover/ext/" + token
	}
	return rawURL
}

// proxiedTrackCover 把曲目封面改写为 /api/v1/cover/{ref} 代理路径，
// 与房间广播（room.go）同一不变量：cover_url 一律为服务端代理路径。
func proxiedTrackCover(t *provider.Track) {
	if t.CoverURL == "" || strings.HasPrefix(t.CoverURL, "/") {
		return
	}
	t.CoverURL = "/api/v1/cover/" + url.PathEscape(t.Ref.String())
}

// proxiedSearchTracks 就地改写一批曲目的封面为代理路径。
func proxiedSearchTracks(tracks []provider.Track) {
	for i := range tracks {
		proxiedTrackCover(&tracks[i])
	}
}

// proxiedSearchResults 就地改写分类检索结果：song 条目改曲目封面，
// 实体条目签发实体封面 token。
func (s *Server) proxiedSearchResults(providerID string, results []provider.SearchResult) {
	for i := range results {
		if results[i].Track != nil {
			proxiedTrackCover(results[i].Track)
			continue
		}
		results[i].CoverURL = s.proxiedEntityCover(providerID, results[i].CoverURL)
	}
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
