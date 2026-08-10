package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
		s.internalError(w, r, "list playlists", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": playlists})
}

// createPlaylist 创建歌单（非 Guest 身份；Guest 是任意昵称自声明身份，
// 不授予歌单创建权——见 requireNonGuest）。
func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireNonGuest(w, r)
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
		s.internalError(w, r, "create playlist", err)
		return
	}
	s.audit(r.Context(), id.ID, "playlist.create", pl.ID, nil)
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
		s.internalError(w, r, "check provider playlist binding", err)
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
		s.internalError(w, r, "create bound playlist", err)
		return
	}
	synced, err := plsync.SyncOne(r.Context(), s.st, s.reg, s.coverSigner, pl.ID)
	if err != nil {
		if rollbackErr := s.st.DeletePlaylist(r.Context(), pl.ID); rollbackErr != nil {
			s.providerError(w, r, "sync bound playlist and roll back", fmt.Errorf(
				"sync provider playlist: %w; rollback failed: %v", err, rollbackErr,
			))
			return
		}
		s.providerError(w, r, "sync bound playlist", err)
		return
	}
	pl, err = s.st.GetPlaylist(r.Context(), pl.ID)
	if err != nil {
		s.internalError(w, r, "load bound playlist", err)
		return
	}
	s.audit(r.Context(), identity.ID, "playlist.bind", pl.ID, map[string]any{"count": synced})
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
	synced, err := plsync.SyncOne(r.Context(), s.st, s.reg, s.coverSigner, pl.ID)
	if err != nil {
		s.providerError(w, r, "sync provider playlist", err)
		return
	}
	s.audit(r.Context(), identity.ID, "playlist.sync", pl.ID, map[string]any{"count": synced})
	writeJSON(w, http.StatusOK, map[string]any{"synced": synced})
}

// detachPlaylist 解除 provider 绑定，保留当前歌单条目。
func (s *Server) detachPlaylist(w http.ResponseWriter, r *http.Request) {
	identity, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if pl.BoundProvider == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "playlist is not provider-bound")
		return
	}
	if err := s.st.ClearPlaylistBinding(r.Context(), pl.ID); err != nil {
		s.internalError(w, r, "detach provider playlist", err)
		return
	}
	s.audit(r.Context(), identity.ID, "playlist.detach", pl.ID, nil)
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
		s.internalError(w, r, "list playlist items", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playlist": pl, "items": items, "offset": offset, "limit": limit,
	})
}

const maxPlaylistCoverBytes int64 = 8 << 20

// setPlaylistCover 为自建歌单保存上传封面。
func (s *Server) setPlaylistCover(w http.ResponseWriter, r *http.Request) {
	identity, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if pl.BoundProvider != "" {
		writeErr(w, http.StatusConflict, "playlist_bound",
			"playlist is provider-bound; detach first")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPlaylistCoverBytes)
	if err := r.ParseMultipartForm(maxPlaylistCoverBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "file field required")
		return
	}
	defer file.Close()

	var sniff [512]byte
	n, readErr := file.Read(sniff[:])
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid image file")
		return
	}
	if contentType := http.DetectContentType(sniff[:n]); !strings.HasPrefix(contentType, "image/") {
		writeErr(w, http.StatusBadRequest, "bad_request", "file must be an image")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid image file")
		return
	}

	if s.playlistCoverDir == "" {
		writeErr(w, http.StatusInternalServerError, "internal", "playlist cover directory is not configured")
		return
	}
	coverDir, err := filepath.Abs(s.playlistCoverDir)
	if err != nil {
		s.internalError(w, r, "resolve playlist cover directory", err)
		return
	}
	if err := os.MkdirAll(coverDir, 0o755); err != nil {
		s.internalError(w, r, "create playlist cover directory", err)
		return
	}
	target := filepath.Join(coverDir, pl.ID)
	tmp, err := os.CreateTemp(coverDir, "."+pl.ID+".tmp-*")
	if err != nil {
		s.internalError(w, r, "create playlist cover temporary file", err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	written, err := io.Copy(tmp, io.LimitReader(file, maxPlaylistCoverBytes+1))
	if err != nil {
		tmp.Close()
		s.internalError(w, r, "write playlist cover temporary file", err)
		return
	}
	if written > maxPlaylistCoverBytes {
		tmp.Close()
		writeErr(w, http.StatusBadRequest, "bad_request", "image exceeds 8MB limit")
		return
	}
	if err := tmp.Close(); err != nil {
		s.internalError(w, r, "close playlist cover temporary file", err)
		return
	}
	if err := os.Rename(tmpPath, target); err != nil {
		s.internalError(w, r, "install playlist cover", err)
		return
	}

	coverURL := "/api/v1/cover/playlist/" + pl.ID
	if err := s.st.SetPlaylistCover(r.Context(), pl.ID, coverURL, target); err != nil {
		s.internalError(w, r, "persist playlist cover", err)
		return
	}
	s.audit(r.Context(), identity.ID, "playlist.cover.set", pl.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"cover_url": coverURL})
}

// clearPlaylistCover 清除自建歌单封面。
func (s *Server) clearPlaylistCover(w http.ResponseWriter, r *http.Request) {
	identity, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if pl.BoundProvider != "" {
		writeErr(w, http.StatusConflict, "playlist_bound",
			"playlist is provider-bound; detach first")
		return
	}
	if err := s.st.SetPlaylistCover(r.Context(), pl.ID, "", ""); err != nil {
		s.internalError(w, r, "clear playlist cover", err)
		return
	}
	if pl.CoverPath != "" {
		if err := os.Remove(pl.CoverPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.internalError(w, r, "remove playlist cover file", err)
			return
		}
	}
	s.audit(r.Context(), identity.ID, "playlist.cover.clear", pl.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// playlistCover 公开提供已上传的歌单封面。
func (s *Server) playlistCover(w http.ResponseWriter, r *http.Request) {
	pl, err := s.st.GetPlaylist(r.Context(), r.PathValue("id"))
	if err != nil || pl.CoverPath == "" {
		writeErr(w, http.StatusNotFound, "not_found", "no cover")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000")
	http.ServeFile(w, r, pl.CoverPath)
}

// updatePlaylistMeta 更新歌单元数据（创建者或 media_admin）：name/description/pinned
// 任意子集，缺省字段保持不变。description 可显式置空；name 不可为空。
// 绑定歌单的名字归外部歌单所有（同步时被远程名覆盖），改名须先 detach——
// 但 description/pinned 属本地状态，绑定歌单允许单独修改。
func (s *Server) updatePlaylistMeta(w http.ResponseWriter, r *http.Request) {
	identity, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plID := pl.ID
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Pinned      *bool   `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if body.Name == nil && body.Description == nil && body.Pinned == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "no fields to update")
		return
	}
	if body.Name != nil {
		trimmed := strings.TrimSpace(*body.Name)
		if trimmed == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		if pl.BoundProvider != "" {
			writeErr(w, http.StatusConflict, "playlist_bound",
				"playlist is provider-bound; detach to rename")
			return
		}
		body.Name = &trimmed
	}
	patch := store.PlaylistPatch{Name: body.Name, Description: body.Description, Pinned: body.Pinned}
	if err := s.st.UpdatePlaylistMeta(r.Context(), plID, patch); err != nil {
		s.internalError(w, r, "update playlist", err)
		return
	}
	s.audit(r.Context(), identity.ID, "playlist.update", plID, map[string]any{
		"name": body.Name != nil, "description": body.Description != nil, "pinned": body.Pinned != nil,
	})
	updated, err := s.st.GetPlaylist(r.Context(), plID)
	if err != nil {
		s.internalError(w, r, "reload playlist", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlist": updated})
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plID := pl.ID
	if err := s.st.DeletePlaylist(r.Context(), plID); err != nil {
		s.internalError(w, r, "delete playlist", err)
		return
	}
	s.audit(r.Context(), id.ID, "playlist.delete", plID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": plID})
}

// playlistItemFromTrack 把 provider 曲目快照成歌单条目（与 plsync 落库格式一致）。
// 只存基础字段会让歌单电台/详情丢失封面等富字段——线上复现：导入的 ncm
// 歌单条目 cover_url 为空，作电台源时前端无封面。
func playlistItemFromTrack(track provider.Track, now int64) store.PlaylistItem {
	contrib := ""
	if len(track.Contributors) > 0 {
		if b, err := json.Marshal(track.Contributors); err == nil {
			contrib = string(b)
		}
	}
	return store.PlaylistItem{
		TrackRef:         track.Ref.String(),
		Title:            track.Title,
		Artist:           track.Artist,
		DurationMs:       track.DurationMs,
		Album:            track.Album,
		CoverURL:         track.CoverURL,
		SourceURL:        track.SourceURL,
		ContributorsJSON: contrib,
		AddedAt:          now,
	}
}

// addPlaylistItems 追加曲目（创建者或 media_admin）。body: {"track_refs": [...]}。
// 每个 ref 经对应 provider GetTrack 取元数据快照。
func (s *Server) addPlaylistItems(w http.ResponseWriter, r *http.Request) {
	id, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plID := pl.ID
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
			s.providerError(w, r, "fetch playlist track "+ref, err)
			return
		}
		items = append(items, playlistItemFromTrack(track, now))
	}
	if err := s.st.AppendPlaylistItems(r.Context(), plID, items); err != nil {
		s.internalError(w, r, "append playlist items", err)
		return
	}
	s.audit(r.Context(), id.ID, "playlist.add_items", plID, map[string]any{"count": len(items)})
	writeJSON(w, http.StatusOK, map[string]any{"added": len(items)})
}

func (s *Server) deletePlaylistItem(w http.ResponseWriter, r *http.Request) {
	id, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	ord, err := strconv.Atoi(r.PathValue("ord"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid ord")
		return
	}
	plID := pl.ID
	if pl.BoundProvider != "" {
		writeErr(w, http.StatusConflict, "playlist_bound",
			"playlist is provider-bound; detach first")
		return
	}
	if err := s.st.DeletePlaylistItem(r.Context(), plID, ord); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "item not found")
		return
	}
	s.audit(r.Context(), id.ID, "playlist.delete_item", plID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": ord})
}

// movePlaylistItem 移动条目到 to_ord（创建者或 media_admin）。body: {"to_ord": N}。
// to_ord clamp 到 [1, len]；移动后序号重排保持连续。
func (s *Server) movePlaylistItem(w http.ResponseWriter, r *http.Request) {
	id, pl, ok := s.requirePlaylistManager(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plID := pl.ID
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
		s.internalError(w, r, "move playlist item", err)
		return
	}
	s.audit(r.Context(), id.ID, "playlist.move_item", plID, map[string]any{
		"ord": ord, "to_ord": finalOrd,
	})
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
				s.providerError(w, r, "materialize playlist source", err)
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
		name, _, tracks, err = imp.ImportPlaylist(ctx, body.PlaylistID)
		if err != nil {
			s.providerError(w, r, "import provider playlist", err)
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
		s.internalError(w, r, "create imported playlist", err)
		return
	}
	items := make([]store.PlaylistItem, 0, len(tracks))
	for _, t := range tracks {
		items = append(items, playlistItemFromTrack(t, now))
	}
	if err := s.st.AppendPlaylistItems(ctx, pl.ID, items); err != nil {
		s.internalError(w, r, "append imported playlist items", err)
		return
	}
	s.audit(r.Context(), id.ID, "playlist.import", pl.ID, map[string]any{"count": len(items)})
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
