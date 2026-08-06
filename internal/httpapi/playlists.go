package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/plsync"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// ---------- 歌单管理 ----------

func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	playlists, err := s.st.ListPlaylists(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": playlists})
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}
	now := nowMs()
	pl := store.Playlist{
		ID: "pl_" + randHex(6), Name: body.Name, Description: body.Description,
		CreatedBy: id.ID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreatePlaylist(r.Context(), pl); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "playlist.create", pl.ID, "{}")
	writeJSON(w, http.StatusCreated, map[string]any{"playlist": pl})
}

// bindPlaylist 绑定外部 provider 歌单，并在创建时完成首次全量同步。
func (s *Server) bindPlaylist(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	var body struct {
		Provider   string `json:"provider"`
		PlaylistID string `json:"playlist_id"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	body.Provider = strings.TrimSpace(body.Provider)
	body.PlaylistID = strings.TrimSpace(body.PlaylistID)
	if body.Provider == "" || body.PlaylistID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider and playlist_id required")
		return
	}
	p, ok := s.reg.Get(body.Provider)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown provider: "+body.Provider)
		return
	}
	if _, ok := p.(provider.PlaylistImporter); !ok {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"provider does not support playlist import: "+body.Provider)
		return
	}
	existing, found, err := s.st.GetPlaylistByBinding(r.Context(), body.Provider, body.PlaylistID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if found {
		writeErr(w, http.StatusConflict, "already_bound",
			"provider playlist is already bound to "+existing.ID)
		return
	}

	now := nowMs()
	pl := store.Playlist{
		ID:            "pl_" + randHex(6),
		Name:          body.Name,
		CreatedBy:     identity.ID,
		CreatedAt:     now,
		UpdatedAt:     now,
		BoundProvider: body.Provider,
		BoundRemoteID: body.PlaylistID,
	}
	if err := s.st.CreatePlaylist(r.Context(), pl); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	synced, err := plsync.SyncOne(r.Context(), s.st, s.reg, pl.ID)
	if err != nil {
		if rollbackErr := s.st.DeletePlaylist(r.Context(), pl.ID); rollbackErr != nil {
			writeErr(w, http.StatusBadGateway, "provider_error",
				err.Error()+"; rollback failed: "+rollbackErr.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	pl, err = s.st.GetPlaylist(r.Context(), pl.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), identity.ID, "playlist.bind", pl.ID,
		`{"count":`+strconv.Itoa(synced)+`}`)
	writeJSON(w, http.StatusOK, map[string]any{"playlist": pl, "synced": synced})
}

// syncPlaylist 立即全量同步 provider 绑定歌单。
func (s *Server) syncPlaylist(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	pl, err := s.st.GetPlaylist(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	if pl.BoundProvider == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "playlist is not provider-bound")
		return
	}
	synced, err := plsync.SyncOne(r.Context(), s.st, s.reg, pl.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	s.st.Audit(r.Context(), identity.ID, "playlist.sync", pl.ID,
		`{"count":`+strconv.Itoa(synced)+`}`)
	writeJSON(w, http.StatusOK, map[string]any{"synced": synced})
}

// detachPlaylist 解除 provider 绑定，保留当前歌单条目。
func (s *Server) detachPlaylist(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	pl, err := s.st.GetPlaylist(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	if pl.BoundProvider == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "playlist is not provider-bound")
		return
	}
	if err := s.st.ClearPlaylistBinding(r.Context(), pl.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), identity.ID, "playlist.detach", pl.ID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"detached": pl.ID})
}

func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	id := r.PathValue("id")
	pl, err := s.st.GetPlaylist(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "playlist not found")
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
	items, err := s.st.PlaylistItems(r.Context(), id, offset, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playlist": pl, "items": items, "offset": offset, "limit": limit,
	})
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	plID := r.PathValue("id")
	if err := s.st.DeletePlaylist(r.Context(), plID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "playlist.delete", plID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": plID})
}

// addPlaylistItems 追加曲目（media_admin）。body: {"track_refs": [...]}。
// 每个 ref 经对应 provider GetTrack 取元数据快照。
func (s *Server) addPlaylistItems(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	plID := r.PathValue("id")
	pl, err := s.st.GetPlaylist(r.Context(), plID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	if pl.BoundProvider != "" {
		writeErr(w, http.StatusConflict, "playlist_bound",
			"playlist is provider-bound; detach first")
		return
	}
	var body struct {
		TrackRefs []string `json:"track_refs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TrackRefs) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "track_refs required")
		return
	}
	if len(body.TrackRefs) > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "at most 100 tracks per call")
		return
	}
	now := nowMs()
	items := make([]store.PlaylistItem, 0, len(body.TrackRefs))
	for _, ref := range body.TrackRefs {
		p, _, err := s.reg.ForRef(provider.TrackRef(ref))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		track, err := p.GetTrack(r.Context(), provider.TrackRef(ref))
		if err != nil {
			writeErr(w, http.StatusBadGateway, "provider_error", ref+": "+err.Error())
			return
		}
		items = append(items, store.PlaylistItem{
			TrackRef: track.Ref.String(), Title: track.Title,
			Artist: track.Artist, DurationMs: track.DurationMs, AddedAt: now,
		})
	}
	if err := s.st.AppendPlaylistItems(r.Context(), plID, items); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "playlist.add_items", plID,
		`{"count":`+strconv.Itoa(len(items))+`}`)
	writeJSON(w, http.StatusOK, map[string]any{"added": len(items)})
}

func (s *Server) deletePlaylistItem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	ord, err := strconv.Atoi(r.PathValue("ord"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid ord")
		return
	}
	plID := r.PathValue("id")
	pl, err := s.st.GetPlaylist(r.Context(), plID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	if pl.BoundProvider != "" {
		writeErr(w, http.StatusConflict, "playlist_bound",
			"playlist is provider-bound; detach first")
		return
	}
	if err := s.st.DeletePlaylistItem(r.Context(), plID, ord); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "item not found")
		return
	}
	s.st.Audit(r.Context(), id.ID, "playlist.delete_item", plID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": ord})
}

// movePlaylistItem 移动条目到 to_ord（media_admin）。body: {"to_ord": N}。
// to_ord clamp 到 [1, len]；移动后序号重排保持连续。
func (s *Server) movePlaylistItem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	plID := r.PathValue("id")
	pl, err := s.st.GetPlaylist(r.Context(), plID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	if pl.BoundProvider != "" {
		writeErr(w, http.StatusConflict, "playlist_bound",
			"playlist is provider-bound; detach first")
		return
	}
	ord, err := strconv.Atoi(r.PathValue("ord"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid ord")
		return
	}
	var body struct {
		ToOrd *int `json:"to_ord"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ToOrd == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "to_ord required")
		return
	}
	finalOrd, err := s.st.MovePlaylistItem(r.Context(), plID, ord, *body.ToOrd)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "item not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "playlist.move_item", plID,
		`{"ord":`+strconv.Itoa(ord)+`,"to_ord":`+strconv.Itoa(finalOrd)+`}`)
	writeJSON(w, http.StatusOK, map[string]any{"moved": ord, "to_ord": finalOrd})
}

// importPlaylist 导入外部歌单或曲目源快照（media_admin）。
// body: {"provider": "ncm", "playlist_id": "12345 或 URL"}   — 外部歌单
// 或:   {"source": "ncm:daily"}                              — 曲目源物化
// 可选: {"name": "自定义歌单名"}
func (s *Server) importPlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	var body struct {
		Provider   string `json:"provider"`
		PlaylistID string `json:"playlist_id"`
		Source     string `json:"source"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var name string
	var tracks []provider.Track

	switch {
	case body.Source != "":
		// 曲目源物化：循环取批直到耗尽（封顶 500 首防失控）
		src, err := s.sourceFromSpec(ctx, body.Source)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		name = src.Description()
		for len(tracks) < 500 {
			batch, exhausted, err := src.NextBatch(ctx, 50, "")
			if err != nil {
				writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
				return
			}
			tracks = append(tracks, batch...)
			if exhausted || len(batch) == 0 {
				break
			}
		}
	case body.Provider != "" && body.PlaylistID != "":
		p, okp := s.reg.Get(body.Provider)
		if !okp {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown provider: "+body.Provider)
			return
		}
		imp, oki := p.(provider.PlaylistImporter)
		if !oki {
			writeErr(w, http.StatusBadRequest, "bad_request", "provider does not support playlist import: "+body.Provider)
			return
		}
		var err error
		name, tracks, err = imp.ImportPlaylist(ctx, body.PlaylistID)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "either source or provider+playlist_id required")
		return
	}

	if body.Name != "" {
		name = body.Name
	}
	now := nowMs()
	pl := store.Playlist{
		ID: "pl_" + randHex(6), Name: name,
		CreatedBy: id.ID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreatePlaylist(ctx, pl); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	items := make([]store.PlaylistItem, 0, len(tracks))
	for _, t := range tracks {
		items = append(items, store.PlaylistItem{
			TrackRef: t.Ref.String(), Title: t.Title,
			Artist: t.Artist, DurationMs: t.DurationMs, AddedAt: now,
		})
	}
	if err := s.st.AppendPlaylistItems(ctx, pl.ID, items); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.st.Audit(r.Context(), id.ID, "playlist.import", pl.ID,
		`{"count":`+strconv.Itoa(len(items))+`}`)
	pl.TrackCount = len(items)
	writeJSON(w, http.StatusCreated, map[string]any{"playlist": pl})
}

// sourceFromSpec 仅供导入使用（房间 radio 的解析在 room 包）。
func (s *Server) sourceFromSpec(ctx context.Context, spec string) (provider.TrackSource, error) {
	pid, rest, err := provider.TrackRef(spec).Split()
	if err != nil {
		return nil, err
	}
	if pid == "playlist" {
		return nil, fmt.Errorf("cannot import a playlist into a playlist")
	}
	p, ok := s.reg.Get(pid)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", pid)
	}
	factory, ok := p.(provider.SourceFactory)
	if !ok {
		return nil, fmt.Errorf("provider %q does not provide track sources", pid)
	}
	return factory.NewSource(ctx, rest)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
