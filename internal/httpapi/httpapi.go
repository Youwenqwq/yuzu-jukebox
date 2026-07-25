// Package httpapi 实现 REST /api/v1 与出流 /stream/v1。
package httpapi

import (
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
}

func NewServer(st *store.Store, authm *auth.Manager, rooms *room.Manager, reg *provider.Registry, lp *local.Provider, c *cache.Cache, ws *wsapi.Server) *Server {
	return &Server{st: st, authm: authm, rooms: rooms, reg: reg, local: lp, cache: c, ws: ws}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/guest", s.guestAuth)
	mux.HandleFunc("GET /api/v1/rooms", s.listRooms)
	mux.HandleFunc("POST /api/v1/rooms", s.createRoom)
	mux.HandleFunc("PATCH /api/v1/rooms/{id}", s.updateRoom)
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("POST /api/v1/media/upload", s.upload)
	mux.HandleFunc("GET /api/v1/media/cache", s.listCache)
	mux.HandleFunc("DELETE /api/v1/media/cache/{ref}", s.evictCache)
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

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleListener); !ok {
		return
	}
	type roomInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := []roomInfo{}
	for _, rm := range s.rooms.List() {
		out = append(out, roomInfo{ID: rm.ID, Name: rm.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	var body struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		GuestPassword string `json:"guest_password"`
		Policy       string `json:"policy"`
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
	s.st.Audit(r.Context(), id.ID, "room.update", roomID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"room": map[string]any{"id": roomID, "name": name}})
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
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
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
