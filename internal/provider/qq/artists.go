package qq

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail 实现 provider.ArtistDetailer：艺人名 → 歌手搜索首条
// （/search/search_by_type search_type=1）→ /singer/{mid}/desc 取头像与简介。
// 名字在 QQ 侧不存在时返回错误，httpapi 降级为纯本地统计。
// 该端点匿名可用（web 层 AuthPolicy.NONE），凭据仅用于写操作。
func (p *Provider) ArtistDetail(ctx context.Context, name string) (provider.ArtistDetail, error) {
	q := url.Values{
		"keyword":     {name},
		"search_type": {"1"},
		"page":        {"1"},
		"num":         {"5"},
		"highlight":   {"false"},
	}
	var data struct {
		Singer []qqSingerSearch `json:"singer"`
	}
	if err := p.get(ctx, p.client, "/search/search_by_type", q, nil, &data); err != nil {
		return provider.ArtistDetail{}, err
	}
	if len(data.Singer) == 0 {
		return provider.ArtistDetail{}, fmt.Errorf("qq singer %q not found", name)
	}
	mid := data.Singer[0].Mid
	if mid == "" {
		return provider.ArtistDetail{}, fmt.Errorf("qq singer %q: empty mid", name)
	}

	var desc struct {
		SingerList []struct {
			BasicInfo struct {
				Name string `json:"name"`
			} `json:"basic_info"`
			ExInfo struct {
				Desc string `json:"desc"`
			} `json:"ex_info"`
			Pic struct {
				Pic string `json:"pic"`
			} `json:"pic"`
		} `json:"singer_list"`
	}
	if err := p.get(ctx, p.client, "/singer/"+url.PathEscape(mid)+"/desc", nil, nil, &desc); err != nil {
		return provider.ArtistDetail{}, err
	}
	if len(desc.SingerList) == 0 {
		return provider.ArtistDetail{}, fmt.Errorf("qq singer %q: empty desc", mid)
	}
	s := desc.SingerList[0]
	detail := provider.ArtistDetail{
		Name:      s.BasicInfo.Name,
		AvatarURL: s.Pic.Pic,
		Bio:       strings.TrimSpace(s.ExInfo.Desc),
	}
	if detail.Name == "" {
		detail.Name = name
	}
	return detail, nil
}

var _ provider.ArtistDetailer = (*Provider)(nil)
