package bili

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail 实现 provider.ArtistDetailer：艺人名 → /search/up 首条 UP主 →
// 档案富化。两级策略：
//   - 无凭据：直接用搜索快照的 face/sign（匿名可用，-412 风控时失败降级）；
//   - 有凭据：升级为 /space/acc/info 的权威档案（上游要求 SESSDATA cookie
//     ≥3 项，匿名 -403/-404）；升级失败保留搜索快照，不阻断。
//
// 名字在 B 站侧不存在时返回错误，httpapi 降级为纯本地统计。
func (p *Provider) ArtistDetail(ctx context.Context, name string) (provider.ArtistDetail, error) {
	cookie := p.cookie.Load().(string)
	var searchResp struct {
		Results []struct {
			Mid  int64  `json:"mid"`
			Name string `json:"name"`
			Face string `json:"face"`
			Sign string `json:"sign"`
		} `json:"results"`
	}
	q := url.Values{"keywords": {name}, "limit": {"5"}, "pn": {"1"}}
	if err := p.get(ctx, "/search/up", q, cookie, &searchResp); err != nil {
		return provider.ArtistDetail{}, err
	}
	if len(searchResp.Results) == 0 || searchResp.Results[0].Mid == 0 {
		return provider.ArtistDetail{}, fmt.Errorf("bili up %q not found", name)
	}
	first := searchResp.Results[0]
	detail := provider.ArtistDetail{
		Name:      first.Name,
		AvatarURL: normalizeCoverURL(first.Face),
		Bio:       first.Sign,
	}

	if strings.TrimSpace(cookie) != "" {
		var acc struct {
			Name string `json:"name"`
			Face string `json:"face"`
			Sign string `json:"sign"`
		}
		if err := p.get(ctx, "/space/acc/info", url.Values{
			"mid": {strconv.FormatInt(first.Mid, 10)},
		}, cookie, &acc); err == nil {
			if acc.Name != "" {
				detail.Name = acc.Name
			}
			if acc.Face != "" {
				detail.AvatarURL = normalizeCoverURL(acc.Face)
			}
			if acc.Sign != "" {
				detail.Bio = acc.Sign
			}
		}
	}
	if detail.Name == "" {
		detail.Name = name
	}
	return detail, nil
}

var _ provider.ArtistDetailer = (*Provider)(nil)
