package ncm

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ArtistDetail implements provider.ArtistDetailer by resolving the first artist
// name-search result, then fetching that entity directly by its provider-native ID.
func (p *Provider) ArtistDetail(ctx context.Context, name string) (provider.ArtistDetail, error) {
	var searchResp struct {
		Result struct {
			Artists []struct {
				ID        int64  `json:"id"`
				Name      string `json:"name"`
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

	searchArtist := searchResp.Result.Artists[0]
	detail, err := p.ArtistDetailByID(ctx, strconv.FormatInt(searchArtist.ID, 10))
	if err != nil {
		return provider.ArtistDetail{}, err
	}
	if detail.Name == "" {
		detail.Name = searchArtist.Name
		if detail.Name == "" {
			detail.Name = name
		}
	}
	if detail.AvatarURL == "" {
		detail.AvatarURL = searchArtist.PicURL
	}
	if detail.AvatarURL == "" {
		detail.AvatarURL = searchArtist.Img1v1URL
	}
	return detail, nil
}

// ArtistDetailByID implements provider.ArtistIDDetailer without performing a
// name search. The optional artist-description request only enriches an entity
// whose detail response has no brief description; its failure leaves Bio empty.
func (p *Provider) ArtistDetailByID(ctx context.Context, entityID string) (provider.ArtistDetail, error) {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return provider.ArtistDetail{}, fmt.Errorf("ncm artist: empty entity id")
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
	if err := p.get(ctx, "/artist/detail", url.Values{"id": {entityID}}, "", &detailResp); err != nil {
		return provider.ArtistDetail{}, err
	}

	artist := detailResp.Data.Artist
	avatarURL := artist.PicURL
	if avatarURL == "" {
		avatarURL = artist.Cover
	}
	bio := strings.TrimSpace(artist.BriefDesc)
	if bio == "" {
		var descResp struct {
			Introduction []struct {
				Title string `json:"ti"`
				Text  string `json:"txt"`
			} `json:"introduction"`
		}
		if err := p.get(ctx, "/artist/desc", url.Values{"id": {entityID}}, "", &descResp); err == nil {
			paragraphs := make([]string, 0, len(descResp.Introduction))
			for _, intro := range descResp.Introduction {
				parts := make([]string, 0, 2)
				if title := strings.TrimSpace(intro.Title); title != "" {
					parts = append(parts, title)
				}
				if text := strings.TrimSpace(intro.Text); text != "" {
					parts = append(parts, text)
				}
				if len(parts) != 0 {
					paragraphs = append(paragraphs, strings.Join(parts, "\n"))
				}
			}
			bio = strings.Join(paragraphs, "\n\n")
		}
	}
	return provider.ArtistDetail{
		Name:      artist.Name,
		EntityID:  entityID,
		AvatarURL: avatarURL,
		Bio:       bio,
	}, nil
}

var (
	_ provider.ArtistDetailer   = (*Provider)(nil)
	_ provider.ArtistIDDetailer = (*Provider)(nil)
)
