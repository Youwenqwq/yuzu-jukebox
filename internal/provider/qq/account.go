package qq

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

// 账号写操作实现 provider.AccountWriter。
// 授权（acting Principal == 凭据 owner）由调用方在 API 层完成；
// 实现只负责用内部凭据执行，凭据永不下发。

// likeDirID 是 QQ"我喜欢的音乐"歌单的固定目录 ID。
const likeDirID = 201

// songMeta 回读曲目数字 id 与类型（账号写操作需要，TrackRef 只有 mid）。
func (p *Provider) songMeta(ctx context.Context, mid string) (qqSong, error) {
	song, _, err := p.songDetail(ctx, mid)
	return song, err
}

// write 用短超时客户端执行账号写操作（POST + 查询参数，Cookie 凭据）。
// 信封 code==0 即成功；布尔写接口失败时返回 code=-1。
func (p *Provider) write(ctx context.Context, path string, q url.Values) error {
	cred := p.cred.Load()
	if !cred.hasLogin() {
		return fmt.Errorf("qq api %s: credential not configured", path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", cred.cookieHeader())
	return p.do(p.writeClient, req, path, nil)
}

// Like 将曲目加入"我喜欢的音乐"（dirid=201）。
func (p *Provider) Like(ctx context.Context, id string) error {
	return p.playlistWrite(ctx, likeDirID, 0, id)
}

// LikeCheck 回读喜欢状态：拉取"我喜欢的音乐"全量（分页），按 mid 比对。
func (p *Provider) LikeCheck(ctx context.Context, id string) (bool, error) {
	cred := p.cred.Load()
	if !cred.hasLogin() {
		return false, fmt.Errorf("qq api /user/{euin}/fav/songs: credential not configured")
	}
	if cred.EncryptUin == "" {
		return false, fmt.Errorf("qq credential account unavailable: re-login required")
	}
	const perPage = 100
	for page := 1; ; page++ {
		var data struct {
			Songs   []qqSong `json:"songs"`
			Hasmore int      `json:"hasmore"`
		}
		if err := p.get(ctx, p.client, "/user/"+url.PathEscape(cred.EncryptUin)+"/fav/songs",
			url.Values{"num": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}, cred, &data); err != nil {
			return false, err
		}
		for _, s := range data.Songs {
			if s.Mid == id {
				return true, nil
			}
		}
		if data.Hasmore == 0 || len(data.Songs) == 0 {
			break
		}
		if page >= 100 { // 防御
			break
		}
	}
	return false, nil
}

// playlistRef 是 AccountPlaylist.ID 的复合格式 "tid:dirid"。
// QQ 的 add_songs 需要同时提供歌单 TID 与目录 ID，单值不够。
func parsePlaylistRef(playlistID string) (tid, dirid int64, err error) {
	tidStr, diridStr, ok := strings.Cut(playlistID, ":")
	if !ok {
		return 0, 0, fmt.Errorf("qq playlist ref %q: want \"tid:dirid\"", playlistID)
	}
	tid, err = strconv.ParseInt(tidStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("qq playlist ref %q: bad tid", playlistID)
	}
	dirid, err = strconv.ParseInt(diridStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("qq playlist ref %q: bad dirid", playlistID)
	}
	return tid, dirid, nil
}

// AddToPlaylist 将曲目加入凭据账号的指定歌单（playlistID 为 "tid:dirid"）。
func (p *Provider) AddToPlaylist(ctx context.Context, playlistID, trackID string) error {
	tid, dirid, err := parsePlaylistRef(playlistID)
	if err != nil {
		return err
	}
	return p.playlistWrite(ctx, dirid, tid, trackID)
}

func (p *Provider) playlistWrite(ctx context.Context, dirid, tid int64, trackID string) error {
	cred := p.cred.Load()
	if !cred.hasLogin() {
		return fmt.Errorf("qq api /songlist/add_songs: credential not configured")
	}
	song, err := p.songMeta(ctx, trackID)
	if err != nil {
		return err
	}
	songType := song.Type
	if songType <= 0 {
		songType = 1 // 普通歌曲
	}
	q := url.Values{
		"dirid":     {strconv.FormatInt(dirid, 10)},
		"song_id":   {strconv.FormatInt(song.ID, 10)},
		"song_type": {strconv.Itoa(songType)},
		"tid":       {strconv.FormatInt(tid, 10)},
	}
	return p.write(ctx, "/songlist/add_songs", q)
}

// AccountPlaylists 列出凭据账号的歌单（创建 + 收藏，去重）。
// ID 编码为 "tid:dirid" 复合格式。
func (p *Provider) AccountPlaylists(ctx context.Context) ([]provider.AccountPlaylist, error) {
	cred := p.cred.Load()
	if !cred.hasLogin() {
		return nil, fmt.Errorf("qq api /user/{uin}/created_songlists: credential not configured")
	}
	out := make([]provider.AccountPlaylist, 0, 8)
	seen := make(map[string]bool)

	// 创建的歌单（路径用数字 uin = musicid）。
	created, err := p.fetchPlaylistPages(ctx, "/user/"+strconv.FormatInt(cred.MusicID, 10)+"/created_songlists", cred)
	if err != nil {
		return nil, err
	}
	for _, pl := range created {
		id := playlistRef(pl.ID, pl.DirID)
		seen[id] = true
		out = append(out, provider.AccountPlaylist{
			ID:         id,
			Name:       pl.Title,
			CoverURL:   pl.Picurl,
			TrackCount: pl.Songnum,
		})
	}

	// 收藏的歌单（路径用加密 uin；无 encrypt_uin 时跳过，尽力而为）。
	if cred.EncryptUin != "" {
		if fav, err := p.fetchPlaylistPages(ctx, "/user/"+url.PathEscape(cred.EncryptUin)+"/fav/songlists", cred); err == nil {
			for _, pl := range fav {
				id := playlistRef(pl.ID, pl.DirID)
				if seen[id] {
					continue
				}
				seen[id] = true
				out = append(out, provider.AccountPlaylist{
					ID:         id,
					Name:       pl.Title,
					CoverURL:   pl.Picurl,
					TrackCount: pl.Songnum,
				})
			}
		}
	}
	return out, nil
}

// qqPlaylistSummary 是歌单摘要（created/fav 共用字段）。
type qqPlaylistSummary struct {
	ID      int64  `json:"id"`
	DirID   int64  `json:"dirid"`
	Title   string `json:"title"`
	Picurl  string `json:"picurl"`
	Songnum int    `json:"songnum"`
}

// fetchPlaylistPages 分页拉取用户歌单列表（created: total 驱动；fav: hasmore 驱动）。
func (p *Provider) fetchPlaylistPages(ctx context.Context, path string, cred *qqCredential) ([]qqPlaylistSummary, error) {
	const perPage = 100
	var out []qqPlaylistSummary
	for page := 1; ; page++ {
		var data struct {
			Total     int                 `json:"total"`
			Hasmore   int                 `json:"hasmore"`
			Playlists []qqPlaylistSummary `json:"playlists"`
		}
		if err := p.get(ctx, p.client, path,
			url.Values{"num": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}, cred, &data); err != nil {
			return nil, err
		}
		out = append(out, data.Playlists...)
		if (data.Hasmore == 0 && data.Total <= 0) || len(data.Playlists) == 0 {
			break
		}
		if data.Total > 0 && len(out) >= data.Total {
			break
		}
		if page >= 50 { // 防御
			break
		}
	}
	return out, nil
}

func playlistRef(tid, dirid int64) string {
	return strconv.FormatInt(tid, 10) + ":" + strconv.FormatInt(dirid, 10)
}
