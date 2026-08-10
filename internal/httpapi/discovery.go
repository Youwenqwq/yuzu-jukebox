// discovery.go 首页漫游的只读端点：艺人档案与推荐 feed。
package httpapi

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// artistProfileLimit 艺人档案的「热门曲目」固定取前 10（卡片容量上限）。
const artistProfileLimit = 10

// artistProfileResponse 艺人档案端点响应体。AvatarURL/Bio 来自 provider
// 富化（最佳努力，缺失即省略）；TopTracks 为本地 play_history 聚合。
type artistProfileResponse struct {
	Name         string                  `json:"name"`
	PlayCount    int                     `json:"play_count"`
	LastPlayedAt int64                   `json:"last_played_at"`
	AvatarURL    string                  `json:"avatar_url,omitempty"`
	Bio          string                  `json:"bio,omitempty"`
	TopTracks    []store.ArtistTrackStat `json:"top_tracks"`
}

// artistProfile 艺人档案：本地播放统计（"月听众"替身）+ 热门曲目，
// 再由首个支持 ArtistDetailer 的 provider 最佳努力富化头像/简介。
// 名字是唯一键（play_history 与前端卡片都只有名字），富化失败不降级数据。
func (s *Server) artistProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRequester); !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}
	profile, err := s.st.ArtistProfile(r.Context(), name, artistProfileLimit)
	if err != nil {
		s.internalError(w, r, "load artist profile", err)
		return
	}
	// 历史不存封面：热门曲目统一发代理路径，封面端点按 ref 现场解析。
	for i := range profile.TopTracks {
		profile.TopTracks[i].CoverURL = "/api/v1/cover/" + url.PathEscape(profile.TopTracks[i].TrackRef)
	}
	resp := artistProfileResponse{
		Name:         profile.Name,
		PlayCount:    profile.PlayCount,
		LastPlayedAt: profile.LastPlayedAt,
		TopTracks:    profile.TopTracks,
	}
	if detail, ok := s.enrichArtist(r.Context(), name); ok {
		resp.AvatarURL = detail.AvatarURL
		resp.Bio = detail.Bio
	}
	writeJSON(w, http.StatusOK, map[string]any{"artist": resp})
}

// enrichArtist 用首个支持 ArtistDetailer 的 provider 解析艺人档案富信息。
// 任何失败（provider 不可用、名字不存在、能力缺席）都跳过富化；
// 头像 URL 走实体封面代理签发（与搜索实体同一机制，防伪造 token）。
func (s *Server) enrichArtist(ctx context.Context, name string) (provider.ArtistDetail, bool) {
	for _, p := range s.reg.All() {
		ad, ok := p.(provider.ArtistDetailer)
		if !ok {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		detail, err := ad.ArtistDetail(dctx, name)
		cancel()
		if err != nil || strings.TrimSpace(detail.Name) == "" {
			continue
		}
		detail.AvatarURL = s.proxiedEntityCover(p.ID(), detail.AvatarURL)
		return detail, true
	}
	return provider.ArtistDetail{}, false
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
