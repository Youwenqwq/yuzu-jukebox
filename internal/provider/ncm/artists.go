package ncm

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail 实现 provider.ArtistDetailer：把艺人名解析为歌手实体。
// 名字 → 实体 ID 的映射走 type=100 搜索取首条，再取 /artist/detail 的简介；
// 头像优先用详情字段，并回退搜索快照（部分艺人的详情 picUrl 为空）。
// 名字在 NCM 侧不存在时返回错误，httpapi 会继续尝试其它 Provider。
func (p *Provider) ArtistDetail(ctx context.Context, name string) (provider.ArtistDetail, error) {
	var searchResp struct {
		Result struct {
			Artists []struct {
				ID        int64  `json:"id"`
				PicURL    string `json:"picUrl"`
				Img1v1URL string `json:"img1v1Url"`
			} `json:"artists"`
		} `json:"result"`
	}
	if err := p.get(ctx, "/search", url.Values{
		"keywords": {name},
		"type":     {"100"},
		"limit":    {"1"},
	}, "", &searchResp); err != nil {
		return provider.ArtistDetail{}, err
	}
	if len(searchResp.Result.Artists) == 0 {
		return provider.ArtistDetail{}, fmt.Errorf("ncm artist %q not found", name)
	}

	var detailResp struct {
		Data struct {
			Artist struct {
				Name      string `json:"name"`
				PicURL    string `json:"picUrl"`
				Cover     string `json:"cover"`
				BriefDesc string `json:"briefDesc"`
			} `json:"artist"`
		} `json:"data"`
	}
	searchArtist := searchResp.Result.Artists[0]
	entityID := strconv.FormatInt(searchArtist.ID, 10)
	if err := p.get(ctx, "/artist/detail", url.Values{
		"id": {entityID},
	}, "", &detailResp); err != nil {
		return provider.ArtistDetail{}, err
	}
	avatarURL := detailResp.Data.Artist.PicURL
	if avatarURL == "" {
		avatarURL = detailResp.Data.Artist.Cover
	}
	if avatarURL == "" {
		avatarURL = searchArtist.PicURL
	}
	if avatarURL == "" {
		avatarURL = searchArtist.Img1v1URL
	}
	return provider.ArtistDetail{
		Name:      detailResp.Data.Artist.Name,
		EntityID:  entityID,
		AvatarURL: avatarURL,
		Bio:       detailResp.Data.Artist.BriefDesc,
	}, nil
}
