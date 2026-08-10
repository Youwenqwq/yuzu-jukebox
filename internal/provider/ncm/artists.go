package ncm

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail 实现 provider.ArtistDetailer：把艺人名解析为档案细节。
// 名字 → 实体 ID 的映射走 type=100 搜索取首条，再取 /artist/detail 的头像与简介。
// 名字在 NCM 侧不存在时返回错误，httpapi 降级为纯本地统计。
func (p *Provider) ArtistDetail(ctx context.Context, name string) (provider.ArtistDetail, error) {
	var searchResp struct {
		Result struct {
			Artists []struct {
				ID int64 `json:"id"`
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
				BriefDesc string `json:"briefDesc"`
			} `json:"artist"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/artist/detail", url.Values{
		"id": {strconv.FormatInt(searchResp.Result.Artists[0].ID, 10)},
	}, "", &detailResp); err != nil {
		return provider.ArtistDetail{}, err
	}
	return provider.ArtistDetail{
		Name:      detailResp.Data.Artist.Name,
		AvatarURL: detailResp.Data.Artist.PicURL,
		Bio:       detailResp.Data.Artist.BriefDesc,
	}, nil
}
