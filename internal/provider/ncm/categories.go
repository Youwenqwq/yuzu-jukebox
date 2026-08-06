package ncm

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// SearchCategories 报告 NCM 支持的分类检索轴。
func (p *Provider) SearchCategories() []provider.SearchCategory {
	return []provider.SearchCategory{
		provider.SearchCategorySong,
		provider.SearchCategoryArtist,
		provider.SearchCategoryAlbum,
		provider.SearchCategoryPlaylist,
	}
}

// SearchCategory 按分类搜索歌曲或可继续钻取的实体。
func (p *Provider) SearchCategory(ctx context.Context, cat provider.SearchCategory, query string) ([]provider.SearchResult, error) {
	if cat == provider.SearchCategorySong {
		tracks, err := p.Search(ctx, query)
		if err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, len(tracks))
		for i := range tracks {
			results[i] = provider.SearchResult{Type: provider.SearchCategorySong, Track: &tracks[i]}
		}
		return results, nil
	}

	q := url.Values{"keywords": {query}, "limit": {"30"}}
	switch cat {
	case provider.SearchCategoryArtist:
		q.Set("type", "100")
		var resp struct {
			Result struct {
				Artists []struct {
					ID        int64  `json:"id"`
					Name      string `json:"name"`
					PicURL    string `json:"picUrl"`
					Img1v1URL string `json:"img1v1Url"`
					BriefDesc string `json:"briefDesc"`
					AccountID int64  `json:"accountId"`
				} `json:"artists"`
			} `json:"result"`
		}
		if err := p.get(ctx, "/search", q, "", &resp); err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, 0, len(resp.Result.Artists))
		for _, artist := range resp.Result.Artists {
			coverURL := artist.PicURL
			if coverURL == "" {
				coverURL = artist.Img1v1URL
			}
			detail := artist.BriefDesc
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryArtist,
				EntityID: strconv.FormatInt(artist.ID, 10),
				Name:     artist.Name,
				Detail:   detail,
				CoverURL: coverURL,
			})
		}
		return results, nil

	case provider.SearchCategoryAlbum:
		q.Set("type", "10")
		var resp struct {
			Result struct {
				Albums []struct {
					ID     int64  `json:"id"`
					Name   string `json:"name"`
					PicURL string `json:"picUrl"`
					Artist struct {
						Name string `json:"name"`
					} `json:"artist"`
					Artists []struct {
						Name string `json:"name"`
					} `json:"artists"`
				} `json:"albums"`
			} `json:"result"`
		}
		if err := p.get(ctx, "/search", q, "", &resp); err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, 0, len(resp.Result.Albums))
		for _, album := range resp.Result.Albums {
			detail := album.Artist.Name
			if detail == "" {
				detail = joinArtists(namesOf(album.Artists))
			}
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryAlbum,
				EntityID: strconv.FormatInt(album.ID, 10),
				Name:     album.Name,
				Detail:   detail,
				CoverURL: album.PicURL,
			})
		}
		return results, nil

	case provider.SearchCategoryPlaylist:
		q.Set("type", "1000")
		var resp struct {
			Result struct {
				Playlists []struct {
					ID          int64  `json:"id"`
					Name        string `json:"name"`
					TrackCount  int    `json:"trackCount"`
					CoverImgURL string `json:"coverImgUrl"`
				} `json:"playlists"`
			} `json:"result"`
		}
		if err := p.get(ctx, "/search", q, "", &resp); err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, 0, len(resp.Result.Playlists))
		for _, playlist := range resp.Result.Playlists {
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryPlaylist,
				EntityID: strconv.FormatInt(playlist.ID, 10),
				Name:     playlist.Name,
				Detail:   fmt.Sprintf("%d 首", playlist.TrackCount),
				CoverURL: playlist.CoverImgURL,
			})
		}
		return results, nil

	default:
		return nil, provider.ErrNotSupported
	}
}

// EntityTracks 将歌手或专辑实体展开为可入队曲目。歌单继续使用 ImportPlaylist。
func (p *Provider) EntityTracks(ctx context.Context, cat provider.SearchCategory, entityID string) ([]provider.Track, error) {
	var path string
	switch cat {
	case provider.SearchCategoryArtist:
		path = "/artist/top/song"
	case provider.SearchCategoryAlbum:
		path = "/album"
	default:
		return nil, provider.ErrNotSupported
	}

	var resp struct {
		Songs []ncmEntitySong `json:"songs"`
	}
	if err := p.get(ctx, path, url.Values{"id": {entityID}}, "", &resp); err != nil {
		return nil, err
	}
	tracks := make([]provider.Track, 0, len(resp.Songs))
	for _, song := range resp.Songs {
		tracks = append(tracks, p.entitySongTrack(song))
	}
	return tracks, nil
}

type ncmEntitySong struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Duration int64  `json:"duration"`
	Dt       int64  `json:"dt"`
	Al       ncmAl  `json:"al"`
	Album    ncmAl  `json:"album"`
	Artists  []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Ar []struct {
		Name string `json:"name"`
	} `json:"ar"`
}

func (p *Provider) entitySongTrack(song ncmEntitySong) provider.Track {
	duration := song.Duration
	if duration == 0 {
		duration = song.Dt
	}
	album := song.Al
	if album.Name == "" && album.PicURL == "" {
		album = song.Album
	}
	artists := song.Artists
	if len(artists) == 0 {
		artists = song.Ar
	}
	return provider.Track{
		Ref:          provider.NewRef(p.ID(), strconv.FormatInt(song.ID, 10)),
		Title:        song.Name,
		Artist:       joinArtists(namesOf(artists)),
		DurationMs:   duration,
		Album:        album.Name,
		CoverURL:     album.PicURL,
		SourceURL:    sourceURL(song.ID),
		Contributors: artistContributors(artists),
	}
}
