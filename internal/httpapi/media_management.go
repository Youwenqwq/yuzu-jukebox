package httpapi

import (
	"errors"
	"net/http"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/local"
)

type localMediaResponse struct {
	TrackRef   string `json:"track_ref"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"duration_ms"`
	SizeBytes  int64  `json:"size_bytes"`
	UploadedBy string `json:"uploaded_by"`
	CreatedAt  int64  `json:"created_at"`
}

func (s *Server) listMedia(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	files, err := s.local.List(r.Context())
	if err != nil {
		s.internalError(w, r, "list local media", err)
		return
	}
	media := make([]localMediaResponse, 0, len(files))
	for _, file := range files {
		media = append(media, localMediaResponse{
			TrackRef:   provider.NewRef(s.local.ID(), file.ID).String(),
			Title:      file.Title,
			Artist:     file.Artist,
			DurationMs: file.DurationMs,
			SizeBytes:  file.SizeBytes,
			UploadedBy: file.UploadedBy,
			CreatedAt:  file.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"media": media})
}

func (s *Server) deleteMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	ref := provider.TrackRef(r.PathValue("ref"))
	if err := s.local.Delete(r.Context(), ref); err != nil {
		switch {
		case errors.Is(err, local.ErrInvalidRef):
			writeErr(w, http.StatusBadRequest, "bad_request", "only local media can be deleted")
		case errors.Is(err, local.ErrNotFound):
			writeErr(w, http.StatusNotFound, "not_found", "media not found")
		default:
			s.internalError(w, r, "delete local media", err)
		}
		return
	}
	if err := s.cache.EvictTrack(r.Context(), ref); err != nil && !errors.Is(err, cache.ErrNotFound) {
		s.internalError(w, r, "evict deleted media from cache", err)
		return
	}
	s.audit(r.Context(), id.ID, "media.delete", ref.String(), nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": ref.String()})
}
