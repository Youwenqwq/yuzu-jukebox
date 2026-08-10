package ncm

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// ---------- 歌单导入（provider.PlaylistImporter） ----------

var playlistIDRe = regexp.MustCompile(`(?:playlist\?.*id=)?(\d+)`)

// ImportPlaylist 拉取 NCM 歌单全量曲目及封面。接受裸 id 或完整 URL。
// 用 /playlist/track/all 分页（/playlist/detail 的 tracks 只有前 10 首）。
func (p *Provider) ImportPlaylist(ctx context.Context, playlistID string) (string, string, []provider.Track, error) {
	m := playlistIDRe.FindStringSubmatch(playlistID)
	if m == nil {
		return "", "", nil, fmt.Errorf("cannot parse playlist id from %q", playlistID)
	}
	id := m[1]

	// 歌单名
	var detail struct {
		Playlist struct {
			Name        string `json:"name"`
			CoverImgURL string `json:"coverImgUrl"`
		} `json:"playlist"`
	}
	if err := p.get(ctx, "/playlist/detail", url.Values{"id": {id}}, p.cookie.Load().(string), &detail); err != nil {
		return "", "", nil, err
	}
	name := detail.Playlist.Name
	if name == "" {
		name = "ncm playlist " + id
	}

	// 分页拉全量
	const pageSize = 1000
	var tracks []provider.Track
	for offset := 0; ; offset += pageSize {
		var resp struct {
			Songs []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
				Dt   int64  `json:"dt"`
				Al   ncmAl  `json:"al"`
				Ar   []struct {
					Name string `json:"name"`
				} `json:"ar"`
			} `json:"songs"`
		}
		q := url.Values{"id": {id}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := p.get(ctx, "/playlist/track/all", q, p.cookie.Load().(string), &resp); err != nil {
			return "", "", nil, err
		}
		for _, s := range resp.Songs {
			tracks = append(tracks, provider.Track{
				Ref:          provider.NewRef(p.ID(), strconv.FormatInt(s.ID, 10)),
				Title:        s.Name,
				Artist:       joinArtists(namesOf(s.Ar)),
				DurationMs:   s.Dt,
				Album:        s.Al.Name,
				CoverURL:     s.Al.PicURL,
				SourceURL:    sourceURL(s.ID),
				Contributors: artistContributors(s.Ar),
			})
		}
		if len(resp.Songs) < pageSize {
			break
		}
	}
	return name, detail.Playlist.CoverImgURL, tracks, nil
}

// ---------- 曲目源工厂（provider.SourceFactory） ----------

// NewSource 创建曲目源。spec 取值：daily | fm | simi:<id> | heart:<id> | playlist:<id>。
func (p *Provider) NewSource(ctx context.Context, spec string) (provider.TrackSource, error) {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "daily":
		return &dailySource{p: p}, nil
	case "fm":
		return &fmSource{p: p, seen: newSeenSet(500)}, nil
	case "simi":
		if arg == "" {
			return nil, fmt.Errorf("simi source requires a seed: ncm:simi:<song_id>")
		}
		return &chainedSource{p: p, kind: "simi", seed: arg, seen: newSeenSet(500)}, nil
	case "heart":
		if arg == "" {
			return nil, fmt.Errorf("heart source requires a seed: ncm:heart:<song_id>")
		}
		cs := &chainedSource{p: p, kind: "heart", seed: arg, seen: newSeenSet(500)}
		if err := cs.loadLikedPlaylist(ctx); err != nil {
			return nil, fmt.Errorf("heart source: %w", err)
		}
		return cs, nil
	case "playlist":
		if arg == "" {
			return nil, fmt.Errorf("playlist source requires an id: ncm:playlist:<playlist_id>")
		}
		name, _, tracks, err := p.ImportPlaylist(ctx, arg)
		if err != nil {
			return nil, fmt.Errorf("playlist source: %w", err)
		}
		return &listSource{desc: "网易云歌单《" + name + "》", tracks: tracks}, nil
	case "newsong":
		return p.newsongSource(ctx)
	default:
		return nil, fmt.Errorf("unknown ncm source %q (want daily|fm|simi:<id>|heart:<id>|playlist:<id>|newsong)", spec)
	}
}

// newsongSource 物化 /personalized/newsong（匿名推荐新歌，内联完整曲目）。
func (p *Provider) newsongSource(ctx context.Context) (provider.TrackSource, error) {
	var resp struct {
		Result []struct {
			Song ncmEntitySong `json:"song"`
		} `json:"result"`
	}
	if err := p.get(ctx, "/personalized/newsong", url.Values{"limit": {"30"}}, "", &resp); err != nil {
		return nil, fmt.Errorf("newsong source: %w", err)
	}
	tracks := make([]provider.Track, 0, len(resp.Result))
	for _, r := range resp.Result {
		tracks = append(tracks, p.entitySongTrack(r.Song))
	}
	return &listSource{desc: "网易云推荐新歌", tracks: tracks}, nil
}

// RadioSources 返回客户端可配置的网易云电台源。
func (p *Provider) RadioSources() []provider.RadioSource {
	return []provider.RadioSource{
		{Spec: "daily", Name: "每日推荐", Finite: true, RequiresCredential: true},
		{Spec: "newsong", Name: "推荐新歌", Finite: true},
		{Spec: "fm", Name: "私人 FM", Finite: false, RequiresCredential: true},
		{Spec: "simi", Arg: "track_id", Name: "相似歌曲", Finite: false, RequiresCredential: true},
		{Spec: "heart", Arg: "track_id", Name: "心动模式", Finite: false, RequiresCredential: true},
		{Spec: "playlist", Arg: "playlist_id", Name: "歌单电台", Finite: true},
	}
}

// ---------- 外部歌单 / 推荐新歌（一次物化有限源） ----------

type listSource struct {
	desc   string
	tracks []provider.Track

	mu     sync.Mutex
	cursor int
}

func (s *listSource) Description() string { return s.desc }
func (s *listSource) Finite() bool        { return true }
func (s *listSource) Total() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tracks), true
}

func (s *listSource) NextBatch(ctx context.Context, n int, seed provider.TrackRef) ([]provider.Track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor >= len(s.tracks) {
		return nil, true, nil
	}
	end := min(s.cursor+max(n, 0), len(s.tracks))
	batch := s.tracks[s.cursor:end]
	s.cursor = end
	return batch, s.cursor >= len(s.tracks), nil
}

// ---------- 每日推荐（TTL 物化有限源） ----------

type dailySource struct {
	p *Provider

	mu        sync.Mutex
	tracks    []provider.Track
	cursor    int
	fetchedAt string // "2006-01-02"，跨日重拉
}

func (s *dailySource) Description() string { return "网易云每日推荐" }
func (s *dailySource) Finite() bool        { return true }
func (s *dailySource) Total() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tracks), s.fetchedAt != ""
}

func (s *dailySource) NextBatch(ctx context.Context, n int, seed provider.TrackRef) ([]provider.Track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if s.fetchedAt != today || len(s.tracks) == 0 {
		if err := s.refresh(ctx); err != nil {
			return nil, false, err
		}
		s.fetchedAt = today
	}
	if s.cursor >= len(s.tracks) {
		s.cursor = 0 // 循环
	}
	end := min(s.cursor+n, len(s.tracks))
	batch := s.tracks[s.cursor:end]
	s.cursor = end
	return batch, false, nil
}

func (s *dailySource) refresh(ctx context.Context) error {
	var resp struct {
		Data struct {
			DailySongs []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
				Dt   int64  `json:"dt"`
				Al   ncmAl  `json:"al"`
				Ar   []struct {
					Name string `json:"name"`
				} `json:"ar"`
			} `json:"dailySongs"`
		} `json:"data"`
	}
	cookie := s.p.cookie.Load().(string)
	if cookie == "" {
		return fmt.Errorf("daily source requires login (configure ncm credential)")
	}
	if err := s.p.get(ctx, "/recommend/songs", url.Values{}, cookie, &resp); err != nil {
		return err
	}
	s.tracks = s.tracks[:0]
	for _, song := range resp.Data.DailySongs {
		s.tracks = append(s.tracks, provider.Track{
			Ref:          provider.NewRef(s.p.ID(), strconv.FormatInt(song.ID, 10)),
			Title:        song.Name,
			Artist:       joinArtists(namesOf(song.Ar)),
			DurationMs:   song.Dt,
			Album:        song.Al.Name,
			CoverURL:     song.Al.PicURL,
			SourceURL:    sourceURL(song.ID),
			Contributors: artistContributors(song.Ar),
		})
	}
	s.cursor = 0
	return nil
}

// ---------- 私人FM（无限流） ----------

type fmSource struct {
	p    *Provider
	seen *seenSet
}

func (s *fmSource) Description() string { return "网易云私人FM" }
func (s *fmSource) Finite() bool        { return false }

func (s *fmSource) NextBatch(ctx context.Context, n int, seed provider.TrackRef) ([]provider.Track, bool, error) {
	cookie := s.p.cookie.Load().(string)
	if cookie == "" {
		return nil, false, fmt.Errorf("fm source requires login (configure ncm credential)")
	}
	var out []provider.Track
	// personal_fm 每次调用只给一两首，循环取到凑够一批
	for attempt := 0; len(out) < n && attempt < n*2; attempt++ {
		var resp struct {
			Data []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Duration int64  `json:"duration"`
				Album    ncmAl  `json:"album"`
				Artists  []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"data"`
		}
		if err := s.p.get(ctx, "/personal_fm", url.Values{}, cookie, &resp); err != nil {
			return nil, false, err
		}
		for _, song := range resp.Data {
			ref := provider.NewRef(s.p.ID(), strconv.FormatInt(song.ID, 10))
			if s.seen.Add(ref.String()) {
				out = append(out, provider.Track{
					Ref: ref, Title: song.Name,
					Artist: joinArtists(namesOf(song.Artists)), DurationMs: song.Duration,
					Album: song.Album.Name, CoverURL: song.Album.PicURL,
					SourceURL: sourceURL(song.ID), Contributors: artistContributors(song.Artists),
				})
			}
		}
		if len(resp.Data) == 0 {
			break
		}
	}
	return out, false, nil
}

// Similar 一次性查询相似曲目，不复用 chainedSource 的 seed/seen 状态。
func (p *Provider) Similar(ctx context.Context, trackID string, limit int) ([]provider.Track, error) {
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return nil, fmt.Errorf("similar query requires a track id")
	}
	if limit <= 0 {
		limit = 30
	}
	cookie := p.cookie.Load().(string)
	if cookie == "" {
		return nil, fmt.Errorf("similar query requires login (configure ncm credential)")
	}
	tracks, err := p.fetchSimilar(ctx, trackID, cookie)
	if err != nil {
		return nil, err
	}
	if len(tracks) > limit {
		tracks = tracks[:limit]
	}
	return tracks, nil
}

func (p *Provider) fetchSimilar(ctx context.Context, trackID, cookie string) ([]provider.Track, error) {
	var resp struct {
		Songs []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Duration int64  `json:"duration"`
			Album    ncmAl  `json:"album"`
			Artists  []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"songs"`
	}
	if err := p.get(ctx, "/simi/song", url.Values{"id": {trackID}}, cookie, &resp); err != nil {
		return nil, err
	}
	tracks := make([]provider.Track, 0, len(resp.Songs))
	for _, song := range resp.Songs {
		tracks = append(tracks, provider.Track{
			Ref:          provider.NewRef(p.ID(), strconv.FormatInt(song.ID, 10)),
			Title:        song.Name,
			Artist:       joinArtists(namesOf(song.Artists)),
			DurationMs:   song.Duration,
			Album:        song.Album.Name,
			CoverURL:     song.Album.PicURL,
			SourceURL:    sourceURL(song.ID),
			Contributors: artistContributors(song.Artists),
		})
	}
	return tracks, nil
}

// ---------- 链式源（相似歌曲 / 心动模式） ----------

// chainedSource 以房间当前播放曲目为种子的无限游走源。
// simi: /simi/song?id=<当前曲目>；heart: /playmode/intelligence/list?id=<当前曲目>&pid=<我喜欢>。
type chainedSource struct {
	p    *Provider
	kind string // simi | heart
	seed string // 初始种子（规格里的 song id）
	seen *seenSet

	likedPlaylistID string // heart 专用："我喜欢的音乐"歌单 id
}

func (s *chainedSource) Description() string {
	if s.kind == "simi" {
		return "网易云相似歌曲电台"
	}
	return "网易云心动模式"
}
func (s *chainedSource) Finite() bool { return false }

// loadLikedPlaylist 取账号"我喜欢的音乐"歌单 id（/user/playlist 第一张）。
func (s *chainedSource) loadLikedPlaylist(ctx context.Context) error {
	cookie := s.p.cookie.Load().(string)
	if cookie == "" {
		return fmt.Errorf("requires login (configure ncm credential)")
	}
	var status struct {
		Data struct {
			Account struct {
				ID int64 `json:"id"`
			} `json:"account"`
		} `json:"data"`
	}
	if err := s.p.get(ctx, "/login/status", url.Values{}, cookie, &status); err != nil {
		return err
	}
	var resp struct {
		Playlist []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"playlist"`
	}
	q := url.Values{"uid": {strconv.FormatInt(status.Data.Account.ID, 10)}}
	if err := s.p.get(ctx, "/user/playlist", q, cookie, &resp); err != nil {
		return err
	}
	if len(resp.Playlist) == 0 {
		return fmt.Errorf("no playlist found for account")
	}
	s.likedPlaylistID = strconv.FormatInt(resp.Playlist[0].ID, 10)
	return nil
}

func (s *chainedSource) NextBatch(ctx context.Context, n int, seed provider.TrackRef) ([]provider.Track, bool, error) {
	cookie := s.p.cookie.Load().(string)
	if cookie == "" {
		return nil, false, fmt.Errorf("%s source requires login (configure ncm credential)", s.kind)
	}
	// 种子：优先用房间当前曲目（仅当它是本 provider 的曲目），否则用初始种子
	useSeed := s.seed
	if pid, id, err := seed.Split(); err == nil && pid == s.p.ID() && id != "" {
		useSeed = id
	}

	var out []provider.Track
	switch s.kind {
	case "simi":
		tracks, err := s.p.fetchSimilar(ctx, useSeed, cookie)
		if err != nil {
			return nil, false, err
		}
		for _, track := range tracks {
			if s.seen.Add(track.Ref.String()) {
				out = append(out, track)
			}
		}
	case "heart":
		// intelligence/list 的条目形态混合：部分平铺、部分嵌套在 songInfo 里，
		// 且嵌套项用 ar/dt 而非 artists/duration。两种都要处理。
		var resp struct {
			Data []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Duration int64  `json:"duration"`
				Album    ncmAl  `json:"album"`
				Artists  []struct {
					Name string `json:"name"`
				} `json:"artists"`
				SongInfo *struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
					Dt   int64  `json:"dt"`
					Al   ncmAl  `json:"al"`
					Ar   []struct {
						Name string `json:"name"`
					} `json:"ar"`
				} `json:"songInfo"`
			} `json:"data"`
		}
		q := url.Values{"id": {useSeed}, "pid": {s.likedPlaylistID}}
		if err := s.p.get(ctx, "/playmode/intelligence/list", q, cookie, &resp); err != nil {
			return nil, false, err
		}
		for _, item := range resp.Data {
			var t provider.Track
			if si := item.SongInfo; si != nil && si.ID != 0 {
				t = provider.Track{
					Ref:          provider.NewRef(s.p.ID(), strconv.FormatInt(si.ID, 10)),
					Title:        si.Name,
					Artist:       joinArtists(namesOf(si.Ar)),
					DurationMs:   si.Dt,
					Album:        si.Al.Name,
					CoverURL:     si.Al.PicURL,
					SourceURL:    sourceURL(si.ID),
					Contributors: artistContributors(si.Ar),
				}
			} else if item.ID != 0 {
				t = provider.Track{
					Ref:          provider.NewRef(s.p.ID(), strconv.FormatInt(item.ID, 10)),
					Title:        item.Name,
					Artist:       joinArtists(namesOf(item.Artists)),
					DurationMs:   item.Duration,
					Album:        item.Album.Name,
					CoverURL:     item.Album.PicURL,
					SourceURL:    sourceURL(item.ID),
					Contributors: artistContributors(item.Artists),
				}
			} else {
				continue // 无法解析的条目，跳过
			}
			if s.seen.Add(t.Ref.String()) {
				out = append(out, t)
			}
		}
	}

	if len(out) > n {
		out = out[:n]
	}
	// 整批都是已听过的 → 判定耗尽，房间优雅停止电台
	exhausted := len(out) == 0
	return out, exhausted, nil
}

// ---------- seen 集合（防链式游走绕圈） ----------

type seenSet struct {
	mu    sync.Mutex
	cap   int
	order []string
	set   map[string]bool
}

func newSeenSet(cap int) *seenSet {
	return &seenSet{cap: cap, set: map[string]bool{}}
}

// Add 记录一个 ref；返回 true 表示是新见的。
func (s *seenSet) Add(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set[ref] {
		return false
	}
	s.set[ref] = true
	s.order = append(s.order, ref)
	if len(s.order) > s.cap {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
	return true
}
