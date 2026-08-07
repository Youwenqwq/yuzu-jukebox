package qq

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// SearchCategories 报告 QQ 支持的分类检索轴。
func (p *Provider) SearchCategories() []provider.SearchCategory {
	return []provider.SearchCategory{
		provider.SearchCategorySong,
		provider.SearchCategoryArtist,
		provider.SearchCategoryAlbum,
		provider.SearchCategoryPlaylist,
	}
}

// SearchCategory 按分类分页搜索歌曲或可继续钻取的实体。
// 上游 search_type：0=song 1=singer 2=album 3=songlist。
func (p *Provider) SearchCategory(ctx context.Context, cat provider.SearchCategory, query string, limit, offset int) ([]provider.SearchResult, error) {
	if cat == provider.SearchCategorySong {
		tracks, err := p.Search(ctx, query, limit, offset)
		if err != nil {
			return nil, err
		}
		results := make([]provider.SearchResult, len(tracks))
		for i := range tracks {
			results[i] = provider.SearchResult{Type: provider.SearchCategorySong, Track: &tracks[i]}
		}
		return results, nil
	}

	num, page, drop := pageParams(limit, offset)
	q := url.Values{
		"keyword":   {query},
		"page":      {strconv.Itoa(page)},
		"num":       {strconv.Itoa(num)},
		"highlight": {"false"},
	}
	switch cat {
	case provider.SearchCategoryArtist:
		q.Set("search_type", "1")
		var data struct {
			Singer []qqSingerSearch `json:"singer"`
		}
		if err := p.get(ctx, p.client, "/search/search_by_type", q, nil, &data); err != nil {
			return nil, err
		}
		items := slicePage(data.Singer, drop, num)
		results := make([]provider.SearchResult, 0, len(items))
		for _, s := range items {
			cover := s.Pic
			if cover == "" {
				cover = singerCoverURL(s.Mid)
			}
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryArtist,
				EntityID: s.Mid,
				Name:     s.Name,
				Detail:   s.Subtitle,
				CoverURL: cover,
			})
		}
		return results, nil

	case provider.SearchCategoryAlbum:
		q.Set("search_type", "2")
		var data struct {
			Album []qqAlbumSearch `json:"album"`
		}
		if err := p.get(ctx, p.client, "/search/search_by_type", q, nil, &data); err != nil {
			return nil, err
		}
		items := slicePage(data.Album, drop, num)
		results := make([]provider.SearchResult, 0, len(items))
		for _, a := range items {
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryAlbum,
				EntityID: a.Mid,
				Name:     a.Name,
				Detail:   joinSingerNames(a.SingerList),
				CoverURL: a.Pic,
			})
		}
		return results, nil

	case provider.SearchCategoryPlaylist:
		q.Set("search_type", "3")
		var data struct {
			Songlist []qqSonglistSearch `json:"songlist"`
		}
		if err := p.get(ctx, p.client, "/search/search_by_type", q, nil, &data); err != nil {
			return nil, err
		}
		items := slicePage(data.Songlist, drop, num)
		results := make([]provider.SearchResult, 0, len(items))
		for _, pl := range items {
			results = append(results, provider.SearchResult{
				Type:     provider.SearchCategoryPlaylist,
				EntityID: strconv.FormatInt(pl.ID, 10),
				Name:     pl.Title,
				Detail:   fmt.Sprintf("%d 首", pl.Songnum),
				CoverURL: pl.Picurl,
			})
		}
		return results, nil

	default:
		return nil, provider.ErrNotSupported
	}
}

// EntityTracks 将歌手或专辑实体展开为可入队曲目。歌单继续使用 ImportPlaylist。
// 歌手歌曲是上游分页（page/num），专辑歌曲是上游全量返回、本地切片。
func (p *Provider) EntityTracks(ctx context.Context, cat provider.SearchCategory, entityID string, limit, offset int) ([]provider.Track, error) {
	switch cat {
	case provider.SearchCategoryArtist:
		num, page, drop := pageParams(limit, offset)
		q := url.Values{
			"num":  {strconv.Itoa(num)},
			"page": {strconv.Itoa(page)},
		}
		var data struct {
			TotalNum int      `json:"total_num"`
			SongList []qqSong `json:"song_list"`
		}
		if err := p.get(ctx, p.client, "/singer/"+url.PathEscape(entityID)+"/songs", q, nil, &data); err != nil {
			return nil, err
		}
		songs := slicePage(data.SongList, drop, num)
		tracks := make([]provider.Track, 0, len(songs))
		for _, s := range songs {
			tracks = append(tracks, p.songTrack(s))
		}
		return tracks, nil

	case provider.SearchCategoryAlbum:
		limit, offset = normalizeSearchPage(limit, offset)
		var data struct {
			TotalNum int      `json:"total_num"`
			SongList []qqSong `json:"song_list"`
		}
		if err := p.get(ctx, p.client, "/album/"+url.PathEscape(entityID)+"/songs", nil, nil, &data); err != nil {
			return nil, err
		}
		if offset >= len(data.SongList) {
			return []provider.Track{}, nil
		}
		end := len(data.SongList)
		if limit < end-offset {
			end = offset + limit
		}
		songs := data.SongList[offset:end]
		tracks := make([]provider.Track, 0, len(songs))
		for _, s := range songs {
			tracks = append(tracks, p.songTrack(s))
		}
		return tracks, nil

	default:
		return nil, provider.ErrNotSupported
	}
}

// EntityAlbums 将歌手实体展开为可继续钻取的专辑实体列表。
func (p *Provider) EntityAlbums(ctx context.Context, artistID string, limit, offset int) ([]provider.SearchResult, error) {
	num, page, drop := pageParams(limit, offset)
	q := url.Values{
		"num":  {strconv.Itoa(num)},
		"page": {strconv.Itoa(page)},
	}
	var data struct {
		Total     int            `json:"total"`
		AlbumList []qqAlbumBrief `json:"album_list"`
	}
	if err := p.get(ctx, p.client, "/singer/"+url.PathEscape(artistID)+"/albums", q, nil, &data); err != nil {
		return nil, err
	}
	items := slicePage(data.AlbumList, drop, num)
	results := make([]provider.SearchResult, 0, len(items))
	for _, a := range items {
		results = append(results, provider.SearchResult{
			Type:     provider.SearchCategoryAlbum,
			EntityID: a.Mid,
			Name:     a.Name,
			Detail:   a.SingerName,
			CoverURL: albumCoverURL(a.Mid, ""),
		})
	}
	return results, nil
}

// ---------- 实体模型 ----------

// qqSingerSearch 是搜索结果的歌手实体（SingerSearch 序列化子集）。
type qqSingerSearch struct {
	ID       int64  `json:"id"`
	Mid      string `json:"mid"`
	Name     string `json:"name"`
	Pic      string `json:"pic"`
	SongNum  int    `json:"song_num"`
	AlbumNum int    `json:"album_num"`
	MvNum    int    `json:"mv_num"`
	Subtitle string `json:"subtitle"`
}

// qqAlbumSearch 是搜索结果的专辑实体（AlbumSearch 序列化子集）。
// 上游 singer 字段带 <em> 高亮，detail 一律用结构化 singer_list。
type qqAlbumSearch struct {
	ID         int64  `json:"id"`
	Mid        string `json:"mid"`
	Name       string `json:"name"`
	Pic        string `json:"pic"`
	TimePublic string `json:"time_public"`
	SingerList []struct {
		Mid  string `json:"mid"`
		Name string `json:"name"`
	} `json:"singer_list"`
}

// qqSonglistSearch 是搜索结果的歌单实体（SongListSearch 序列化子集）。
type qqSonglistSearch struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Picurl  string `json:"picurl"`
	Songnum int    `json:"songnum"`
}

// qqAlbumBrief 是歌手专辑列表条目（AlbumBrief 序列化子集）。
type qqAlbumBrief struct {
	ID         int64  `json:"id"`
	Mid        string `json:"mid"`
	Name       string `json:"name"`
	TimePublic string `json:"time_public"`
	TotalNum   int    `json:"total_num"`
	SingerName string `json:"singer_name"`
}

// singerCoverURL 按 SDK 的 photo_new 规则拼歌手头像（T001R300x300M000{mid}.jpg）。
func singerCoverURL(mid string) string {
	if mid == "" {
		return ""
	}
	return "https://y.gtimg.cn/music/photo_new/T001R300x300M000" + mid + ".jpg"
}

func joinSingerNames(names []struct {
	Mid  string `json:"mid"`
	Name string `json:"name"`
}) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n.Name != "" {
			out = append(out, n.Name)
		}
	}
	return strings.Join(out, "/")
}
