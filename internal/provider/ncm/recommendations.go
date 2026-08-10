package ncm

import (
	"context"
	"net/url"
	"strconv"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// 推荐 feed 的 shelf 规模：榜单取前 3，每榜取前 10 首。
const (
	recommendationShelfCount  = 3
	recommendationShelfTracks = 10
)

// Recommendations 实现 provider.RecommendationProvider：以 NCM 榜单
// （/toplist → /playlist/track/all）作为首批数据源。单个榜单曲目拉取失败时
// 跳过该 shelf 保留其余（榜单列表本身失败才整体报错）；封面/专辑等富字段
// 取自曲目的 al 对象，序列化层统一改写为代理路径。
//
// 注意：不用 /toplist/detail 物化曲目——实测 enhanced v4.39.0 该端点
// tracks[] 只返回 {first,second} 摘要（无 id/ar/al/dt），直接解码会产出
// ncm:0 空壳曲目；/playlist/track/all 与 ImportPlaylist 同构且带完整歌曲。
func (p *Provider) Recommendations(ctx context.Context) ([]provider.RecommendationShelf, error) {
	var topResp struct {
		List []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := p.get(ctx, "/toplist", nil, "", &topResp); err != nil {
		return nil, err
	}
	top := topResp.List
	if len(top) > recommendationShelfCount {
		top = top[:recommendationShelfCount]
	}

	shelves := make([]provider.RecommendationShelf, 0, len(top))
	var firstErr error
	for _, tl := range top {
		var resp struct {
			Songs []ncmEntitySong `json:"songs"`
		}
		q := url.Values{
			"id":    {strconv.FormatInt(tl.ID, 10)},
			"limit": {strconv.Itoa(recommendationShelfTracks)},
		}
		if err := p.get(ctx, "/playlist/track/all", q, "", &resp); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tracks := resp.Songs
		if len(tracks) > recommendationShelfTracks {
			tracks = tracks[:recommendationShelfTracks]
		}
		out := make([]provider.Track, 0, len(tracks))
		for _, song := range tracks {
			out = append(out, p.entitySongTrack(song))
		}
		shelves = append(shelves, provider.RecommendationShelf{
			ID:     "toplist:" + strconv.FormatInt(tl.ID, 10),
			Title:  tl.Name,
			Tracks: out,
		})
	}
	if len(shelves) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return shelves, nil
}
