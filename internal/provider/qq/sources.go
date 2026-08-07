package qq

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"sync"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ---------- 歌单导入（provider.PlaylistImporter） ----------

var songlistIDRe = regexp.MustCompile(`(\d+)`)

// ImportPlaylist 拉取 QQ 歌单全量曲目及封面。接受裸 id 或完整 URL
// （如 https://y.qq.com/n/ryqq/playlist/1234567890）。
// 用 /songlist/{id}/detail 分页（hasmore 驱动，按 mid 去重）。
func (p *Provider) ImportPlaylist(ctx context.Context, playlistID string) (string, string, []provider.Track, error) {
	m := songlistIDRe.FindString(playlistID)
	if m == "" {
		return "", "", nil, fmt.Errorf("qq playlist id not found in %q", playlistID)
	}
	const perPage = 100
	var name, cover string
	var tracks []provider.Track
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		var data struct {
			Code int `json:"code"`
			Info struct {
				Title  string `json:"title"`
				Picurl string `json:"picurl"`
			} `json:"info"`
			Songs   []qqSong `json:"songs"`
			Total   int      `json:"total"`
			Hasmore int      `json:"hasmore"`
		}
		if err := p.get(ctx, p.client, "/songlist/"+m+"/detail",
			url.Values{"num": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}, nil, &data); err != nil {
			return "", "", nil, err
		}
		if page == 1 {
			name = data.Info.Title
			cover = data.Info.Picurl
		}
		for _, s := range data.Songs {
			if s.Mid == "" || seen[s.Mid] {
				continue
			}
			seen[s.Mid] = true
			tracks = append(tracks, p.songTrack(s))
		}
		if data.Hasmore == 0 || len(data.Songs) == 0 || (data.Total > 0 && len(tracks) >= data.Total) {
			break
		}
		if page >= 500 { // 防御：上游异常时兜底
			break
		}
	}
	if name == "" {
		return "", "", nil, fmt.Errorf("qq playlist %s: empty detail", m)
	}
	return name, cover, tracks, nil
}

// ---------- 曲目源工厂（provider.SourceFactory） ----------

// NewSource 创建曲目源。spec 取值：top:<id>（排行榜，有限）| newsong（新歌，有限）。
// 通用 playlist:<id> 源由 room 层统一处理（DB 游标），不在此列。
func (p *Provider) NewSource(ctx context.Context, spec string) (provider.TrackSource, error) {
	switch {
	case spec == "newsong":
		return &newsongSource{p: p}, nil
	case len(spec) > 4 && spec[:4] == "top:":
		id := spec[4:]
		if id == "" {
			return nil, fmt.Errorf("top source requires an id: qq:top:<top_id>")
		}
		return &topSource{p: p, id: id}, nil
	default:
		return nil, fmt.Errorf("unknown qq source %q (want top:<id>|newsong)", spec)
	}
}

// RadioSources 报告 QQ 支持的电台源（公开、无需凭据）。
func (p *Provider) RadioSources() []provider.RadioSource {
	return []provider.RadioSource{
		{Spec: "newsong", Name: "QQ 新歌推荐", Finite: true},
		{Spec: "top", Arg: "top_id", Name: "QQ 排行榜", Finite: true},
	}
}

// ---------- 排行榜（有限源） ----------

// topSource 分页游走榜单曲目。首请求记录 total_num，供耗尽判定。
type topSource struct {
	p       *Provider
	id      string
	page    int
	fetched int
	total   int
}

func (s *topSource) Description() string { return "QQ 排行榜 " + s.id }
func (s *topSource) Finite() bool        { return true }

func (s *topSource) NextBatch(ctx context.Context, n int, _ provider.TrackRef) ([]provider.Track, bool, error) {
	if n <= 0 {
		n = 10
	}
	s.page++
	q := url.Values{
		"num":  {strconv.Itoa(n)},
		"page": {strconv.Itoa(s.page)},
	}
	var data struct {
		Info struct {
			Name     string `json:"name"`
			TotalNum int    `json:"total_num"`
		} `json:"info"`
		Songs []qqSong `json:"songs"`
	}
	if err := s.p.get(ctx, s.p.client, "/top/"+s.id+"/detail", q, nil, &data); err != nil {
		return nil, false, err
	}
	tracks := make([]provider.Track, 0, len(data.Songs))
	for _, song := range data.Songs {
		tracks = append(tracks, s.p.songTrack(song))
	}
	s.fetched += len(tracks)
	if s.page == 1 && data.Info.TotalNum > 0 {
		s.total = data.Info.TotalNum
	}
	exhausted := len(tracks) < n || (s.total > 0 && s.fetched >= s.total)
	return tracks, exhausted, nil
}

// ---------- 新歌推荐（TTL 物化有限源） ----------

// newsongSource 惰性物化 /recommend/get_recommend_newsong 列表，游标式供给。
type newsongSource struct {
	p *Provider

	mu     sync.Mutex
	tracks []provider.Track
	cursor int
}

func (s *newsongSource) Description() string { return "QQ 新歌推荐" }
func (s *newsongSource) Finite() bool        { return true }

func (s *newsongSource) NextBatch(ctx context.Context, n int, _ provider.TrackRef) ([]provider.Track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tracks) == 0 {
		var data struct {
			Songs []qqSong `json:"songs"`
		}
		if err := s.p.get(ctx, s.p.client, "/recommend/get_recommend_newsong", nil, nil, &data); err != nil {
			return nil, false, err
		}
		s.tracks = make([]provider.Track, 0, len(data.Songs))
		for _, song := range data.Songs {
			if song.Mid != "" {
				s.tracks = append(s.tracks, s.p.songTrack(song))
			}
		}
	}
	if s.cursor >= len(s.tracks) {
		return nil, true, nil
	}
	end := s.cursor + n
	if end > len(s.tracks) {
		end = len(s.tracks)
	}
	batch := s.tracks[s.cursor:end]
	s.cursor = end
	return batch, s.cursor >= len(s.tracks), nil
}
