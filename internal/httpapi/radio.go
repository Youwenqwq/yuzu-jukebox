package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
)

// radioTracks materializes a fresh finite source instance into a paged track list.
func (s *Server) radioTracks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	limit, offset, ok := parseSearchPaging(w, r)
	if !ok {
		return
	}
	if offset > int(^uint(0)>>1)-limit {
		writeErr(w, http.StatusBadRequest, "bad_request", "offset is too large")
		return
	}
	spec := strings.TrimSpace(r.URL.Query().Get("source"))
	if spec == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "source is required")
		return
	}

	source, providerFailure, err := s.newFiniteRadioSource(r.Context(), spec)
	if err != nil {
		if providerFailure {
			s.providerError(w, r, "create radio source", err)
		} else {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	tracks, total, err := materializeRadioSource(r.Context(), source, limit, offset)
	if err != nil {
		s.providerError(w, r, "materialize radio source", err)
		return
	}
	proxiedSearchTracks(tracks)
	writeJSON(w, http.StatusOK, struct {
		Tracks []provider.Track `json:"tracks"`
		Total  *int             `json:"total"`
	}{Tracks: tracks, Total: total})
}

// newFiniteRadioSource validates provider sources against the advertised catalog
// before construction, so unknown and infinite specs are rejected without an
// upstream call. providerFailure distinguishes source construction/upstream errors.
func (s *Server) newFiniteRadioSource(ctx context.Context, spec string) (provider.TrackSource, bool, error) {
	providerID, rest, err := provider.TrackRef(spec).Split()
	if err != nil {
		return nil, false, fmt.Errorf("invalid source %q", spec)
	}
	if providerID == "playlist" {
		source, err := room.NewSourceFromSpec(ctx, spec, s.st, s.reg, false, true)
		return source, false, err
	}

	p, ok := s.reg.Get(providerID)
	if !ok {
		return nil, false, fmt.Errorf("unknown radio source %q", spec)
	}
	catalog, ok := p.(provider.SourceCatalog)
	if !ok {
		return nil, false, fmt.Errorf("unknown radio source %q", spec)
	}
	kind, arg, hasArg := strings.Cut(rest, ":")
	var advertised provider.RadioSource
	found := false
	for _, entry := range catalog.RadioSources() {
		if entry.Spec == kind {
			advertised = entry
			found = true
			break
		}
	}
	if !found || (advertised.Arg == "" && hasArg) ||
		(advertised.Arg != "" && (!hasArg || strings.TrimSpace(arg) == "")) {
		return nil, false, fmt.Errorf("unknown radio source %q", spec)
	}
	if !advertised.Finite {
		return nil, false, fmt.Errorf("radio source %q is not finite", spec)
	}

	source, err := catalog.NewSource(ctx, rest)
	if err != nil {
		return nil, true, err
	}
	if !source.Finite() {
		return nil, false, fmt.Errorf("radio source %q is not finite", spec)
	}
	return source, false, nil
}

func materializeRadioSource(ctx context.Context, source provider.TrackSource, limit, offset int) ([]provider.Track, *int, error) {
	target := offset + limit
	tracks := make([]provider.Track, 0, limit)
	processed := 0
	var totalValue int
	var totalKnown bool

	for processed < target {
		if n, known := radioSourceTotal(source); known {
			totalValue, totalKnown = n, true
			if processed >= n {
				break
			}
		}

		batch, exhausted, err := source.NextBatch(ctx, limit, "")
		if err != nil {
			return nil, nil, err
		}
		batchStart := processed
		processed += len(batch)
		from := max(offset-batchStart, 0)
		to := min(target-batchStart, len(batch))
		if from < to {
			tracks = append(tracks, batch[from:to]...)
		}

		if n, known := radioSourceTotal(source); known {
			totalValue, totalKnown = n, true
			if processed >= n {
				break
			}
		}
		if exhausted {
			if !totalKnown {
				totalValue, totalKnown = processed, true
			}
			break
		}
		if len(batch) == 0 {
			return nil, nil, errors.New("finite radio source returned an empty batch without exhaustion")
		}
	}
	if totalKnown {
		return tracks, new(totalValue), nil
	}
	return tracks, nil, nil
}

func radioSourceTotal(source provider.TrackSource) (int, bool) {
	if totaler, ok := source.(provider.TrackSourceTotaler); ok {
		return totaler.Total()
	}
	return 0, false
}

// similarProviderTracks queries an optional provider capability without
// constructing the chained radio source used by room playback.
func (s *Server) similarProviderTracks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	limit, ok := parseTrackLimit(w, r)
	if !ok {
		return
	}
	trackID := strings.TrimSpace(r.URL.Query().Get("track"))
	if trackID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "track is required")
		return
	}
	providerID := r.PathValue("id")
	p, exists := s.reg.Get(providerID)
	if !exists {
		writeErr(w, http.StatusNotFound, "not_found", "unknown provider: "+providerID)
		return
	}
	querier, supported := p.(provider.SimilarQuerier)
	if !supported {
		writeErr(w, http.StatusNotImplemented, "not_supported", "provider has no similar capability")
		return
	}
	tracks, err := querier.Similar(r.Context(), trackID, limit)
	if errors.Is(err, provider.ErrNotSupported) {
		writeErr(w, http.StatusNotImplemented, "not_supported", "provider has no similar capability")
		return
	}
	if err != nil {
		s.providerError(w, r, "find similar provider tracks", err)
		return
	}
	if tracks == nil {
		tracks = []provider.Track{}
	}
	proxiedSearchTracks(tracks)
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// radioSourceCatalog 枚举 Provider 的动态电台源目录（如 QQ 榜单 top:<id> 全集）。
// 静态 RadioSources() 之外的可选项；能力缺席 501 not_supported。
func (s *Server) radioSourceCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	providerID := r.PathValue("id")
	p, exists := s.reg.Get(providerID)
	if !exists {
		writeErr(w, http.StatusNotFound, "not_found", "unknown provider: "+providerID)
		return
	}
	lister, supported := p.(provider.RadioSourceCatalogLister)
	if !supported {
		writeErr(w, http.StatusNotImplemented, "not_supported", "provider has no radio source catalog")
		return
	}
	entries, err := lister.RadioSourceCatalog(r.Context())
	if err != nil {
		s.providerError(w, r, "list radio source catalog", err)
		return
	}
	if entries == nil {
		entries = []provider.RadioSourceEntry{}
	}
	for i := range entries {
		entries[i].CoverURL = s.proxiedEntityCover(providerID, entries[i].CoverURL)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": entries})
}

func parseTrackLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := 30
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed > 100 {
			writeErr(w, http.StatusBadRequest, "bad_request", "limit must be an integer no greater than 100")
			return 0, false
		}
		if parsed > 0 {
			limit = parsed
		}
	}
	return limit, true
}
