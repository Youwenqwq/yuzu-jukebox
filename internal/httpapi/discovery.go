// discovery.go 首页漫游的只读端点：歌手实体解析与推荐 feed。
package httpapi

import (
	"context"
	"log"
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

// artistProfile 将请求名字解析为 provider 歌手实体。track_ref 仅作为
// Provider 优先级锚点；锚定解析失败后仍按注册表顺序回退其它 Provider。
func (s *Server) artistProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}

	var preferred provider.Provider
	trackRef := strings.TrimSpace(r.URL.Query().Get("track_ref"))
	if trackRef != "" {
		providerID, _, err := provider.TrackRef(trackRef).Split()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid track_ref")
			return
		}
		preferred, _ = s.reg.Get(providerID)
	}

	resp := artistProfileResponse{Name: name}
	if detail, providerID, ok := s.enrichArtist(r.Context(), name, preferred); ok {
		resp.Provider = providerID
		resp.EntityID = detail.EntityID
		resp.AvatarURL = detail.AvatarURL
		resp.Bio = detail.Bio
	}
	writeJSON(w, http.StatusOK, map[string]any{"artist": resp})
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

// recommendations 推荐 feed：聚合所有支持 RecommendationProvider 的 provider
// 的 shelf。单个 provider 失败只记日志跳过（首页 feed 不因一个数据源
// 故障整体 5xx）；无任何数据源时返回空 shelves（200），前端据此隐藏区块。
func (s *Server) recommendations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	// 无任何数据源时输出 [] 而非 null（客户端 shelves.map 不炸）。
	shelves := []provider.RecommendationShelf{}
	for _, p := range s.reg.All() {
		rec, ok := p.(provider.RecommendationProvider)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		got, err := rec.Recommendations(ctx)
		cancel()
		if err != nil {
			log.Printf("recommendations provider %s: %v", p.ID(), err)
			continue
		}
		for i := range got {
			proxiedSearchTracks(got[i].Tracks)
		}
		shelves = append(shelves, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"shelves": shelves})
}
