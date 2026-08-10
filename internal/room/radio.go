package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// ---------- 源规格解析 ----------

// ErrInvalidSource 标记源规格/源配置类错误（映射为 bad_request）。
var ErrInvalidSource = errors.New("invalid source")

// NewSourceFromSpec 把源规格字符串解析成 TrackSource：
//   - "playlist:<id>"       → 通用歌单（DB 游标）
//   - "<provider>:<spec>"   → provider 的 SourceFactory（如 "ncm:daily"）
func NewSourceFromSpec(ctx context.Context, spec string, st *store.Store, reg *provider.Registry, shuffle, once bool) (provider.TrackSource, error) {
	pid, rest, err := provider.TrackRef(spec).Split()
	if err != nil {
		return nil, fmt.Errorf("%w: %q, want \"playlist:<id>\" or \"<provider>:<spec>\"", ErrInvalidSource, spec)
	}
	if pid == "playlist" {
		src, err := newPlaylistSource(st, rest, shuffle, once)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidSource, err)
		}
		return src, nil
	}
	p, ok := reg.Get(pid)
	if !ok {
		return nil, fmt.Errorf("%w: unknown provider %q", ErrInvalidSource, pid)
	}
	factory, ok := p.(provider.SourceFactory)
	if !ok {
		return nil, fmt.Errorf("%w: provider %q does not provide track sources", ErrInvalidSource, pid)
	}
	src, err := factory.NewSource(ctx, rest)
	if err != nil {
		return nil, err
	}
	if !src.Finite() && (shuffle || once) {
		return nil, fmt.Errorf("%w: source %q is an infinite stream; shuffle/once not applicable", ErrInvalidSource, spec)
	}
	return src, nil
}

// ---------- 通用歌单源 ----------

// playlistSource 以 DB 为实时视图的有限源。
// 顺序模式：游标推进，循环回卷或 once 耗尽。
// 随机模式：洗牌袋（全量索引打乱，耗尽重洗或 once 停止）。
type playlistSource struct {
	st    *store.Store
	id    string
	name  string
	total int

	shuffle bool
	once    bool
	cursor  int   // 顺序模式：下一个位置
	bag     []int // 随机模式：剩余索引
	bagUsed bool  // once 随机：洗牌袋已耗尽过一轮
	rng     *rand.Rand
}

func newPlaylistSource(st *store.Store, id string, shuffle, once bool) (*playlistSource, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pl, err := st.GetPlaylist(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("playlist not found: %s", id)
	}
	return &playlistSource{
		st: st, id: id, name: pl.Name, total: pl.TrackCount,
		shuffle: shuffle, once: once,
		rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
	}, nil
}

func (s *playlistSource) Description() string { return "歌单《" + s.name + "》" }
func (s *playlistSource) Finite() bool        { return true }
func (s *playlistSource) Total() (int, bool)  { return s.total, true }

func (s *playlistSource) NextBatch(ctx context.Context, n int, seed provider.TrackRef) ([]provider.Track, bool, error) {
	pl, err := s.st.GetPlaylist(ctx, s.id)
	if err != nil {
		return nil, false, fmt.Errorf("playlist %s: %w", s.id, err)
	}
	count := pl.TrackCount
	s.total = count
	if count == 0 {
		return nil, true, nil
	}
	if s.shuffle {
		return s.batchShuffle(ctx, n, count)
	}
	return s.batchSequential(ctx, n, count)
}

func (s *playlistSource) batchSequential(ctx context.Context, n, count int) ([]provider.Track, bool, error) {
	if s.once && s.cursor >= count {
		return nil, true, nil
	}
	if s.cursor >= count { // 歌单变短了，回卷
		s.cursor = 0
	}
	items, err := s.st.PlaylistItems(ctx, s.id, s.cursor, n)
	if err != nil {
		return nil, false, err
	}
	s.cursor += len(items)
	exhausted := false
	if s.cursor >= count {
		if s.once {
			exhausted = true
		} else {
			// 循环模式：不够一批则回卷补齐
			if len(items) < n {
				more, err := s.st.PlaylistItems(ctx, s.id, 0, n-len(items))
				if err != nil {
					return nil, false, err
				}
				items = append(items, more...)
			}
			s.cursor = s.cursor % count
		}
	}
	return itemsToTracks(items), exhausted, nil
}

func (s *playlistSource) batchShuffle(ctx context.Context, n, count int) ([]provider.Track, bool, error) {
	if len(s.bag) == 0 {
		if s.once && s.bagUsed {
			return nil, true, nil
		}
		s.bag = s.rng.Perm(count)
		s.bagUsed = true
	}
	take := min(n, len(s.bag))
	indices := s.bag[:take]
	s.bag = s.bag[take:]

	var out []provider.Track
	for _, idx := range indices {
		if idx >= count { // 歌单变短，索引失效
			continue
		}
		items, err := s.st.PlaylistItems(ctx, s.id, idx, 1)
		if err != nil {
			return nil, false, err
		}
		out = append(out, itemsToTracks(items)...)
	}
	exhausted := s.once && len(s.bag) == 0
	return out, exhausted, nil
}

func itemsToTracks(items []store.PlaylistItem) []provider.Track {
	out := make([]provider.Track, 0, len(items))
	for _, it := range items {
		t := provider.Track{
			Ref:        provider.TrackRef(it.TrackRef),
			Title:      it.Title,
			Artist:     it.Artist,
			DurationMs: it.DurationMs,
			Album:      it.Album,
			CoverURL:   it.CoverURL,
			SourceURL:  it.SourceURL,
		}
		if it.ContributorsJSON != "" {
			json.Unmarshal([]byte(it.ContributorsJSON), &t.Contributors)
		}
		out = append(out, t)
	}
	return out
}
