package bili

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail implements provider.ArtistDetailer by resolving the first exact
// UP-name search result. The search snapshot remains the fallback when the
// credential is insufficient for, or the request to, /space/acc/info fails.
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
	if !strings.EqualFold(strings.TrimSpace(first.Name), strings.TrimSpace(name)) {
		return provider.ArtistDetail{}, fmt.Errorf("bili up %q not found", name)
	}

	detail := provider.ArtistDetail{
		Name:      first.Name,
		EntityID:  strconv.FormatInt(first.Mid, 10),
		AvatarURL: normalizeCoverURL(first.Face),
		Bio:       first.Sign,
	}
	if artistInfoCookieReady(cookie) {
		if upgraded, err := p.ArtistDetailByID(ctx, detail.EntityID); err == nil {
			if upgraded.Name != "" {
				detail.Name = upgraded.Name
			}
			if upgraded.AvatarURL != "" {
				detail.AvatarURL = upgraded.AvatarURL
			}
			if upgraded.Bio != "" {
				detail.Bio = upgraded.Bio
			}
		}
	}
	if detail.Name == "" {
		detail.Name = name
	}
	return detail, nil
}

// ArtistDetailByID implements provider.ArtistIDDetailer without performing a
// name search. Bilibili's account-info endpoint requires at least three cookie
// items; callers can fall back to the anonymous search snapshot otherwise.
func (p *Provider) ArtistDetailByID(ctx context.Context, entityID string) (provider.ArtistDetail, error) {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return provider.ArtistDetail{}, fmt.Errorf("bili artist: empty entity id")
	}
	cookie := p.cookie.Load().(string)
	if !artistInfoCookieReady(cookie) {
		return provider.ArtistDetail{}, fmt.Errorf("bili artist %q: account detail requires at least 3 cookie items", entityID)
	}

	var acc struct {
		Name string `json:"name"`
		Face string `json:"face"`
		Sign string `json:"sign"`
	}
	if err := p.get(ctx, "/space/acc/info", url.Values{"mid": {entityID}}, cookie, &acc); err != nil {
		return provider.ArtistDetail{}, err
	}
	return provider.ArtistDetail{
		Name:      acc.Name,
		EntityID:  entityID,
		AvatarURL: normalizeCoverURL(acc.Face),
		Bio:       acc.Sign,
	}, nil
}

func artistInfoCookieReady(cookie string) bool {
	pairs := 0
	for _, item := range strings.Split(cookie, ";") {
		key, _, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		pairs++
		if pairs >= 3 {
			return true
		}
	}
	return false
}

var (
	_ provider.ArtistDetailer   = (*Provider)(nil)
	_ provider.ArtistIDDetailer = (*Provider)(nil)
)
