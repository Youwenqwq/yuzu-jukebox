package ncm

import (
	"context"
	"strconv"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

var _ provider.RadioSourceCatalogLister = (*Provider)(nil)

// RadioSourceCatalog implements provider.RadioSourceCatalogLister. NCM's
// /toplist entries are playlists, so each catalog spec can be materialized by
// the existing playlist source machinery.
func (p *Provider) RadioSourceCatalog(ctx context.Context) ([]provider.RadioSourceEntry, error) {
	var resp struct {
		List []struct {
			ID              int64  `json:"id"`
			Name            string `json:"name"`
			CoverImgURL     string `json:"coverImgUrl"`
			UpdateFrequency string `json:"updateFrequency"`
		} `json:"list"`
	}
	if err := p.get(ctx, "/toplist", nil, "", &resp); err != nil {
		return nil, err
	}

	entries := make([]provider.RadioSourceEntry, 0, len(resp.List))
	for _, toplist := range resp.List {
		entries = append(entries, provider.RadioSourceEntry{
			Spec:     "toplist:" + strconv.FormatInt(toplist.ID, 10),
			Name:     toplist.Name,
			CoverURL: toplist.CoverImgURL,
			Detail:   toplist.UpdateFrequency,
		})
	}
	return entries, nil
}
