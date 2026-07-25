// Package httpapi 实现 REST /api/v1 与出流 /stream/v1。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/local"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"

	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	st    *store.Store
	authm *auth.Manager
	rooms *room.Manager
	reg   *provider.Registry
	local *local.Provider
	cache *cache.Cache
	ws    *wsapi.Server

	oidc        *auth.OIDCValidator // nil = OIDC 未启用
	oidcRoleMap map[string][]string
}

func NewServer(st *store.Store, authm *auth.Manager, rooms *room.Manager, reg *provider.Registry, lp *local.Provider, c *cache.Cache, ws *wsapi.Server, oidc *auth.OIDCValidator, oidcRoleMap map[string][]string) *Server {
	return &Server{st: st, authm: authm, rooms: rooms, reg: reg, local: lp, cache: c, ws: ws,
		oidc: oidc, oidcRoleMap: oidcRoleMap}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/guest", s.guestAuth)
	mux.HandleFunc("POST /api/v1/auth/oidc", s.oidcAuth)
	mux.HandleFunc("GET /api/v1/auth/oidc/config", s.oidcConfig)
	mux.HandleFunc("GET /api/v1/rooms", s.listRooms)
	mux.HandleFunc("POST /api/v1/rooms", s.createRoom)
	mux.HandleFunc("PATCH /api/v1/rooms/{id}", s.updateRoom)
	mux.HandleFunc("GET /api/v1/rooms/{id}/history", s.roomHistory)
	mux.HandleFunc("GET /api/v1/rooms/{id}/stats", s.roomStats)
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/providers", s.listProviders)
	mux.HandleFunc("POST /api/v1/providers/{id}/credential", s.setCredential)
	mux.HandleFunc("POST /api/v1/providers/{id}/qrlogin", s.qrLoginStart)
	mux.HandleFunc("GET /api/v1/providers/{id}/qrlogin/{key}", s.qrLoginPoll)
	mux.HandleFunc("POST /api/v1/media/upload", s.upload)
	mux.HandleFunc("GET /api/v1/media/cache", s.listCache)
	mux.HandleFunc("DELETE /api/v1/media/cache/{ref}", s.evictCache)
	mux.HandleFunc("GET /api/v1/playlists", s.listPlaylists)
	mux.HandleFunc("POST /api/v1/playlists", s.createPlaylist)
	mux.HandleFunc("GET /api/v1/playlists/{id}", s.getPlaylist)
	mux.HandleFunc("DELETE /api/v1/playlists/{id}", s.deletePlaylist)
	mux.HandleFunc("POST /api/v1/playlists/{id}/items", s.addPlaylistItems)
	mux.HandleFunc("DELETE /api/v1/playlists/{id}/items/{ord}", s.deletePlaylistItem)
	mux.HandleFunc("POST /api/v1/playlists/import", s.importPlaylist)
	mux.HandleFunc("GET /stream/v1/{ref}", s.stream)
	mux.Handle("/ws/v1", s.ws)
	return mux
}

// ---------- 认证辅助 ----------

type ctxKey int

const ctxIdentity ctxKey = 0

func (s *Server) authenticate(r *http.Request) (auth.Identity, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return auth.Identity{}, auth.ErrSessionNotFound
	}
	return s.authm.Session(tok)
}

func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, role string) (auth.Identity, bool) {
	id, err := s.authenticate(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "login first")
		return id, false
	}
	if !id.HasRole(role) {
		writeErr(w, http.StatusForbidden, "forbidden", "role required: "+role)
		return id, false
	}
	return id, true
}

// ---------- 处理器 ----------

func (s *Server) guestAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	id, token, err := s.authm.GuestAuth(body.Name, body.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity": id, "session_token": token})
}

// oidcConfig 公开 OIDC 客户端配置（issuer/client_id 本就是公开值，
// Native 公共客户端无 secret）。CLI 的 login 流程靠它零配置自发现。
func (s *Server) oidcConfig(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeErr(w, http.StatusNotFound, "not_found", "oidc not enabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": s.oidc.Issuer(), "client_id": s.oidc.ClientID(),
	})
}

// oidcAuth OIDC 登录：客户端递来 IdP 签发的 id_token，
// 服务端验签 + 角色映射后签发 yuzu 会话。未启用 OIDC 时 404。
func (s *Server) oidcAuth(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeErr(w, http.StatusNotFound, "not_found", "oidc not enabled")
		return
	}
	var body struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"` // 可选：id_token 缺 preferred_username 时用于 userinfo 兜底
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IDToken == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "id_token required")
		return
	}
	claims, err := s.oidc.Validate(r.Context(), body.IDToken)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	// Zitadel 颁发 access token 时会把 profile claims 从 id_token 剥掉，
	// 且角色 claim 也可能只在 userinfo 里（Assert Roles on Authentication
	// 作用于 userinfo；id_token 需单独勾 User Roles Inside ID Token）。
	// 有 access_token 就统一走 userinfo：补显示名 + 合并角色。
	if body.AccessToken != "" {
		if info, err := s.oidc.Userinfo(r.Context(), body.AccessToken); err == nil {
			if claims.Username == claims.Sub {
				if name, _ := info["preferred_username"].(string); name != "" {
					claims.Username = name
				} else if name, _ := info["name"].(string); name != "" {
					claims.Username = name
				}
			}
			claims.Roles = mergeRoles(claims.Roles, zitadelRolesFrom(info))
		}
	}
	roles := []string{auth.RoleListener, auth.RoleRequester}
	seen := map[string]bool{auth.RoleListener: true, auth.RoleRequester: true}
	for _, zr := range claims.Roles {
		for _, mapped := range s.oidcRoleMap[zr] {
			if !seen[mapped] {
				seen[mapped] = true
				roles = append(roles, mapped)
			}
		}
	}
	id := auth.OIDCIdentity(claims, roles)
	token := s.authm.IssueSession(id)
	s.st.Audit(r.Context(), id.ID, "auth.oidc", "", `{"name":`+strconv.Quote(id.Name)+`}`)
	writeJSON(w, http.StatusOK, map[string]any{"identity": id, "session_token": token})
}

// mergeRoles 合并去重。
func mergeRoles(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, r := range append(a, b...) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// zitadelRolesFrom 从 userinfo map 提取 Zitadel 角色名。
func zitadelRolesFrom(info map[string]any) []string {
	var out []string
	for k, v := range info {
		if !strings.HasPrefix(k, "urn:zitadel:iam:org:project") || !strings.HasSuffix(k, ":roles") {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			for role := range m {
				out = append(out, role)
			}
		}
	}
	return out
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleListener); !ok {
		return
	}
	type roomInfo struct {
		ID     string          `json:"id"`
		Name   string          `json:"name"`
		Policy json.RawMessage `json:"policy"`
	}
	out := []roomInfo{}
	for _, rm := range s.rooms.List() {
		pol := json.RawMessage(rm.PolicyRaw())
		if len(pol) == 0 {
			pol = json.RawMessage(`{}`)
		}
		out = append(out, roomInfo{ID: rm.ID, Name: rm.Name, Policy: pol})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	var body struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		GuestPassword string `json:"guest_password"`
		Policy        string `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}
	if body.ID == "" {
		body.ID = slugify(body.Name)
	}
	hash := ""
	if body.GuestPassword != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(body.GuestPassword), bcrypt.DefaultCost)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		hash = string(h)
	}
	policy := body.Policy
	if policy == "" {
		policy = "{}"
	}
	row := store.Room{
		ID: body.ID, Name: body.Name, PasswordHash: hash,
		PolicyJSON: policy, CreatedAt: nowMs(),
	}
	if err := s.st.CreateRoom(r.Context(), row); err != nil {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	s.rooms.Spawn(row)
	s.st.Audit(r.Context(), id.ID, "room.create", row.ID, `{"name":`+strconv.Quote(row.Name)+`}`)
	writeJSON(w, http.StatusCreated, map[string]any{"room": map[string]any{"id": row.ID, "name": row.Name}})
}

func (s *Server) updateRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	roomID := r.PathValue("id")
	var body struct {
		Name          *string `json:"name"`
		GuestPassword *string `json:"guest_password"`
		Policy        *string `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	row, err := s.st.GetRoom(r.Context(), roomID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	name := row.Name
	if body.Name != nil {
		name = *body.Name
	}
	hash := row.PasswordHash
	if body.GuestPassword != nil {
		if *body.GuestPassword == "" {
			hash = ""
		} else {
			h, err := bcrypt.GenerateFromPassword([]byte(*body.GuestPassword), bcrypt.DefaultCost)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			hash = string(h)
		}
	}
	if err := s.st.UpdateRoom(r.Context(), roomID, name, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if body.Policy != nil {
		if _, err := room.ParsePolicy(*body.Policy); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		rm, err := s.rooms.Get(roomID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "room not running")
			return
		}
		if err := rm.SetPolicy(*body.Policy); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	s.st.Audit(r.Context(), id.ID, "room.update", roomID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"room": map[string]any{"id": roomID, "name": name}})
}

// roomHistory 房间播放历史（最新在前）。
func (s *Server) roomHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleListener); !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.st.PlayHistory(r.Context(), r.PathValue("id"), offset, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": rows})
}

// roomStats 房间曲目热度榜（首播时间、播放次数、最近播放）。
func (s *Server) roomStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleListener); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	stats, err := s.st.PlayStats(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	type info struct {
		ID               string `json:"id"`
		CredentialStatus string `json:"credential_status,omitempty"` // 仅 CredentialAware provider 有此字段
	}
	out := []info{}
	for _, p := range s.reg.All() {
		entry := info{ID: p.ID()}
		if _, ok := p.(provider.CredentialAware); ok {
			if status, err := s.st.GetCredentialStatus(r.Context(), p.ID()); err == nil {
				entry.CredentialStatus = status
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// setCredential 热更新 provider 凭据（media_admin）。
// body: {"payload": "MUSIC_U=xxx"}
func (s *Server) setCredential(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	p, okp := s.reg.Get(pid)
	if !okp {
		writeErr(w, http.StatusNotFound, "not_found", "unknown provider: "+pid)
		return
	}
	ca, okc := p.(provider.CredentialAware)
	if !okc {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider does not accept credentials: "+pid)
		return
	}
	var body struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	// 校验可能涉及外部调用，放宽超时
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := ca.SetCredential(ctx, body.Payload); err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "provider.set_credential", pid, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"provider": pid, "status": "ok"})
}

// qrLoginStart 生成二维码登录会话（media_admin）。
func (s *Server) qrLoginStart(w http.ResponseWriter, r *http.Request) {
	_, qa, ok := s.requireQRProvider(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	key, content, err := qa.QRLoginStart(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "qr_content": content})
}

// qrLoginPoll 轮询扫码状态；ok 时凭据已由服务端落库并热生效。
func (s *Server) qrLoginPoll(w http.ResponseWriter, r *http.Request) {
	_, qa, ok := s.requireQRProvider(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, message, err := qa.QRLoginPoll(ctx, r.PathValue("key"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "message": message})
}

func (s *Server) requireQRProvider(w http.ResponseWriter, r *http.Request) (auth.Identity, provider.QRLoginAware, bool) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return id, nil, false
	}
	p, okp := s.reg.Get(r.PathValue("id"))
	if !okp {
		writeErr(w, http.StatusNotFound, "not_found", "unknown provider: "+r.PathValue("id"))
		return id, nil, false
	}
	qa, okq := p.(provider.QRLoginAware)
	if !okq {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider does not support qr login")
		return id, nil, false
	}
	return id, qa, true
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	providerID := r.URL.Query().Get("provider")
	if providerID == "" {
		providerID = "local"
	}
	q := r.URL.Query().Get("q")
	p, okp := s.reg.Get(providerID)
	if !okp {
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown provider: "+providerID)
		return
	}
	tracks, err := p.Search(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "file field required")
		return
	}
	defer file.Close()
	var durationMs int64
	if v := r.FormValue("duration_ms"); v != "" {
		durationMs, _ = strconv.ParseInt(v, 10, 64)
	}
	track, err := s.local.Add(r.Context(), header.Filename, file,
		r.FormValue("title"), r.FormValue("artist"), id.ID, durationMs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "media.upload", track.Ref.String(), "{}")
	writeJSON(w, http.StatusCreated, map[string]any{"track": track})
}

func (s *Server) listCache(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	rows, err := s.st.ListCacheRows(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":   rows,
		"downloads": s.cache.Downloads(),
		"history":   s.cache.History(),
	})
}

func (s *Server) evictCache(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	ref := r.PathValue("ref")
	if err := s.cache.EvictTrack(r.Context(), provider.TrackRef(ref)); err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "not cached")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "media.cache_evict", ref, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"evicted": ref})
}

// stream 统一出流。票据鉴权；支持 Range（完整缓存）与首拉流式 tee。
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ref := provider.TrackRef(r.PathValue("ref"))
	ticket := r.URL.Query().Get("ticket")
	if err := s.authm.ValidateTicket(ticket, ref.String()); err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired ticket")
		return
	}

	ext := ""
	if row, err := s.st.GetCacheRow(r.Context(), ref.String()); err == nil {
		ext = filepath.Ext(row.FilePath)
	}
	if ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			w.Header().Set("Content-Type", mt)
		}
	}

	if r.Header.Get("Range") != "" {
		// Range 请求需要完整文件（可能触发同步拉取）
		f, err := s.cache.Open(r.Context(), ref)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
		return
	}

	rc, err := s.cache.OpenStream(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	defer rc.Close()
	w.WriteHeader(http.StatusOK)
	io.Copy(w, rc)
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func slugify(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == ' ', r == '-', r == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "room"
	}
	return string(out)
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
