// discovery.go 首页漫游的歌手实体只读端点。
package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// artistProfileResponse 是名字解析出的 provider 歌手实体。
type artistProfileResponse struct {
	Name      string `json:"name"`
	Provider  string `json:"provider,omitempty"`
	EntityID  string `json:"entity_id,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Bio       string `json:"bio,omitempty"`
}

// artistProfile 将请求名字解析为 provider 歌手实体。显式 provider+entity_id
// 最优先直接解析；失败后依次尝试 track_ref 贡献者实体 ID 和原有名字解析。
func (s *Server) artistProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}

	query := r.URL.Query()
	hasProvider := query.Has("provider")
	hasEntityID := query.Has("entity_id")
	providerID := strings.TrimSpace(query.Get("provider"))
	entityID := strings.TrimSpace(query.Get("entity_id"))
	if hasProvider != hasEntityID || (hasProvider && (providerID == "" || entityID == "")) {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider and entity_id must be provided together")
		return
	}

	var (
		detail             provider.ArtistDetail
		resolvedProviderID string
		resolved           bool
	)
	if hasProvider {
		p, ok := s.reg.Get(providerID)
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", "unknown provider: "+providerID)
			return
		}
		if detail, resolved = s.resolveArtistByID(r.Context(), entityID, p); resolved {
			resolvedProviderID = p.ID()
		}
	}

	var preferred provider.Provider
	var anchoredRef provider.TrackRef
	trackRef := strings.TrimSpace(query.Get("track_ref"))
	if !resolved && trackRef != "" {
		anchoredRef = provider.TrackRef(trackRef)
		trackProviderID, _, err := anchoredRef.Split()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid track_ref")
			return
		}
		preferred, _ = s.reg.Get(trackProviderID)
	}

	if !resolved && preferred != nil {
		if detail, resolved = s.resolveAnchoredArtist(r.Context(), name, anchoredRef, preferred); resolved {
			resolvedProviderID = preferred.ID()
		}
	}
	if !resolved {
		detail, resolvedProviderID, resolved = s.enrichArtist(r.Context(), name, preferred)
	}

	resp := artistProfileResponse{Name: name}
	if resolved {
		resp.Provider = resolvedProviderID
		resp.EntityID = detail.EntityID
		resp.AvatarURL = detail.AvatarURL
		resp.Bio = detail.Bio
	}
	writeJSON(w, http.StatusOK, map[string]any{"artist": resp})
}

func (s *Server) resolveAnchoredArtist(ctx context.Context, name string, trackRef provider.TrackRef, p provider.Provider) (provider.ArtistDetail, bool) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	track, err := p.GetTrack(dctx, trackRef)
	cancel()
	if err != nil {
		return provider.ArtistDetail{}, false
	}
	for _, contributor := range track.Contributors {
		if contributor.Name != name || (contributor.Role != "artist" && contributor.Role != "uploader") {
			continue
		}
		entityID := strings.TrimSpace(contributor.EntityID)
		if entityID == "" {
			continue
		}
		return s.resolveArtistByID(ctx, entityID, p)
	}
	return provider.ArtistDetail{}, false
}

// enrichArtist 先尝试 track_ref 锚定的 Provider，再按 Provider ID 稳定顺序
// 回退全局解析。任何失败或能力缺席都继续；头像统一改写为实体封面代理。
// 多歌手 join 串（如 "塞壬唱片-MSR/F.L.O.A.T (白羽）/张晶"）先按完整串试，
// 失败再逐段（'/' 分割去空白）尝试，取首个成功段。
func (s *Server) enrichArtist(ctx context.Context, name string, preferred provider.Provider) (provider.ArtistDetail, string, bool) {
	for _, candidate := range artistCandidates(name) {
		if detail, ok := s.resolveArtist(ctx, candidate, preferred); ok {
			return detail, preferred.ID(), true
		}
		for _, p := range s.reg.All() {
			if preferred != nil && p.ID() == preferred.ID() {
				continue
			}
			if detail, ok := s.resolveArtist(ctx, candidate, p); ok {
				return detail, p.ID(), true
			}
		}
	}
	return provider.ArtistDetail{}, "", false
}

// artistCandidates 解析候选名字序列：完整名优先，join 串各段（去空白）随后。
// 名字本身含 '/' 的单艺人（如 "AC/DC"）完整名解析通常直接成功，不触发拆段。
func artistCandidates(name string) []string {
	candidates := make([]string, 0, 4)
	candidates = append(candidates, name)
	for _, segment := range strings.Split(name, "/") {
		segment = strings.TrimSpace(segment)
		if segment != "" && segment != name {
			candidates = append(candidates, segment)
		}
	}
	return candidates
}

func (s *Server) resolveArtist(ctx context.Context, name string, p provider.Provider) (provider.ArtistDetail, bool) {
	if p == nil {
		return provider.ArtistDetail{}, false
	}
	ad, ok := p.(provider.ArtistDetailer)
	if !ok {
		return provider.ArtistDetail{}, false
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	detail, err := ad.ArtistDetail(dctx, name)
	cancel()
	if err != nil {
		return provider.ArtistDetail{}, false
	}
	detail.AvatarURL = s.proxiedEntityCover(p.ID(), detail.AvatarURL)
	return detail, true
}

func (s *Server) resolveArtistByID(ctx context.Context, entityID string, p provider.Provider) (provider.ArtistDetail, bool) {
	ad, ok := p.(provider.ArtistIDDetailer)
	if !ok {
		return provider.ArtistDetail{}, false
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	detail, err := ad.ArtistDetailByID(dctx, entityID)
	cancel()
	if err != nil {
		return provider.ArtistDetail{}, false
	}
	detail.AvatarURL = s.proxiedEntityCover(p.ID(), detail.AvatarURL)
	return detail, true
}
