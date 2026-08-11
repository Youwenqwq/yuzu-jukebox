package qq

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail implements provider.ArtistDetailer by resolving the first singer
// search result, then fetching that entity directly by its provider-native ID.
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
	searchSinger := data.Singer[0]
	if searchSinger.Mid == "" {
		return provider.ArtistDetail{}, fmt.Errorf("qq singer %q: empty mid", name)
	}

	detail, err := p.ArtistDetailByID(ctx, searchSinger.Mid)
	if err != nil {
		return provider.ArtistDetail{}, err
	}
	if detail.Name == "" {
		detail.Name = searchSinger.Name
		if detail.Name == "" {
			detail.Name = name
		}
	}
	return detail, nil
}

// ArtistDetailByID implements provider.ArtistIDDetailer without performing a
// name search. The singer description endpoint is anonymously accessible.
func (p *Provider) ArtistDetailByID(ctx context.Context, entityID string) (provider.ArtistDetail, error) {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return provider.ArtistDetail{}, fmt.Errorf("qq singer: empty entity id")
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
	if err := p.get(ctx, p.client, "/singer/"+url.PathEscape(entityID)+"/desc", nil, nil, &desc); err != nil {
		return provider.ArtistDetail{}, err
	}
	if len(desc.SingerList) == 0 {
		return provider.ArtistDetail{}, fmt.Errorf("qq singer %q: empty desc", entityID)
	}
	singer := desc.SingerList[0]
	return provider.ArtistDetail{
		Name:      singer.BasicInfo.Name,
		EntityID:  entityID,
		AvatarURL: singer.Pic.Pic,
		Bio:       strings.TrimSpace(singer.ExInfo.Desc),
	}, nil
}

var (
	_ provider.ArtistDetailer   = (*Provider)(nil)
	_ provider.ArtistIDDetailer = (*Provider)(nil)
)
