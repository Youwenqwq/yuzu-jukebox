// Package plsync 定期同步绑定到外部 provider 的歌单。
package plsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/coverurl"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const (
	importTimeout = 120 * time.Second
	startupDelay  = 30 * time.Second
)

type Syncer struct {
	reg         *provider.Registry
	st          *store.Store
	coverSigner *coverurl.Signer
	interval    time.Duration
}

func New(reg *provider.Registry, st *store.Store, signer *coverurl.Signer) *Syncer {
	return &Syncer{reg: reg, st: st, coverSigner: signer}
}

// SetInterval 设置周期同步间隔；非正值关闭周期同步，不影响手动同步。
func (s *Syncer) SetInterval(d time.Duration) { s.interval = d }

// Run 在短暂启动延迟后同步到期歌单，此后按设置周期重复，直到 ctx 取消。
func (s *Syncer) Run(ctx context.Context) {
	if s.interval <= 0 {
		return
	}

	timer := time.NewTimer(startupDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		s.syncDue(ctx, time.Now())
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.syncDue(ctx, now)
		}
	}
}

func (s *Syncer) syncDue(ctx context.Context, now time.Time) {
	playlists, err := s.st.ListBoundPlaylists(ctx)
	if err != nil {
		log.Printf("[plsync] list bound playlists: %v", err)
		return
	}
	for _, playlist := range playlists {
		if !isDue(playlist, now, s.interval) {
			continue
		}
		if _, err := SyncOne(ctx, s.st, s.reg, s.coverSigner, playlist.ID); err != nil {
			log.Printf("[plsync] %s: sync failed: %v", playlist.ID, err)
		}
	}
}

func isDue(playlist store.Playlist, now time.Time, interval time.Duration) bool {
	return now.UnixMilli()-playlist.LastSyncAt >= interval.Milliseconds()
}

// SyncOne 从绑定的 provider 拉取歌单全量内容并原子替换本地条目。
func SyncOne(ctx context.Context, st *store.Store, reg *provider.Registry, signer *coverurl.Signer, playlistID string) (int, error) {
	playlist, err := st.GetPlaylist(ctx, playlistID)
	if err != nil {
		return 0, err
	}
	if playlist.BoundProvider == "" || playlist.BoundRemoteID == "" {
		return 0, fmt.Errorf("playlist %q is not provider-bound", playlistID)
	}

	p, ok := reg.Get(playlist.BoundProvider)
	if !ok {
		return 0, recordFailure(ctx, st, playlistID,
			fmt.Errorf("unknown provider %q", playlist.BoundProvider))
	}
	imp, ok := p.(provider.PlaylistImporter)
	if !ok {
		return 0, recordFailure(ctx, st, playlistID,
			fmt.Errorf("provider %q does not support playlist import", playlist.BoundProvider))
	}

	importCtx, cancel := context.WithTimeout(ctx, importTimeout)
	name, importedCoverURL, tracks, err := imp.ImportPlaylist(importCtx, playlist.BoundRemoteID)
	cancel()
	if err != nil {
		return 0, recordFailure(ctx, st, playlistID, err)
	}

	now := time.Now().UnixMilli()
	items := make([]store.PlaylistItem, len(tracks))
	for i, track := range tracks {
		contributors, err := json.Marshal(track.Contributors)
		if err != nil {
			return 0, recordFailure(ctx, st, playlistID, fmt.Errorf("marshal contributors: %w", err))
		}
		items[i] = store.PlaylistItem{
			TrackRef:         track.Ref.String(),
			Title:            track.Title,
			Artist:           track.Artist,
			DurationMs:       track.DurationMs,
			Album:            track.Album,
			CoverURL:         track.CoverURL,
			SourceURL:        track.SourceURL,
			ContributorsJSON: string(contributors),
			AddedAt:          now,
		}
	}
	if err := st.ReplacePlaylistItems(ctx, playlistID, items); err != nil {
		return 0, recordFailure(ctx, st, playlistID, err)
	}
	coverURL := ""
	if importedCoverURL != "" && signer != nil {
		if token := signer.Mint(playlist.BoundProvider, importedCoverURL); token != "" {
			coverURL = "/api/v1/cover/ext/" + token
		}
	}
	if err := st.SetPlaylistSyncResult(ctx, playlistID, name, coverURL, now, nil); err != nil {
		return 0, err
	}
	return len(items), nil
}

func recordFailure(ctx context.Context, st *store.Store, playlistID string, syncErr error) error {
	if err := st.SetPlaylistSyncResult(ctx, playlistID, "", "", time.Now().UnixMilli(), syncErr); err != nil {
		return fmt.Errorf("%w (record sync result: %v)", syncErr, err)
	}
	return syncErr
}
