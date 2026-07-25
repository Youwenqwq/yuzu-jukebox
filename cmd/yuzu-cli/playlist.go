// playlist.go — 歌单管理命令实现。
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdPlaylists(ctx context.Context) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	playlists, err := client.RESTListPlaylists(ctx, *server, token)
	if err != nil {
		return err
	}
	if len(playlists) == 0 {
		fmt.Println("(no playlists — create one with: yuzu-cli playlist create <name>)")
		return nil
	}
	for _, p := range playlists {
		fmt.Printf("%-14s %-24s %d 首\n", p.ID, p.Name, p.TrackCount)
	}
	return nil
}

func cmdPlaylistShow(ctx context.Context, id string, offset int) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	pl, items, err := client.RESTGetPlaylist(ctx, *server, token, id, offset, *limit)
	if err != nil {
		return err
	}
	fmt.Printf("%s《%s》 共 %d 首（显示 %d-%d）\n", pl.ID, pl.Name, pl.TrackCount, offset, offset+len(items))
	for _, it := range items {
		fmt.Printf("  %-6d %-24s %-30s %-18s %ds\n", it.Ord, it.TrackRef, it.Title, it.Artist, it.DurationMs/1000)
	}
	return nil
}

func cmdPlaylistCreate(ctx context.Context, plName, desc string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	pl, err := client.RESTCreatePlaylist(ctx, *server, token, plName, desc)
	if err != nil {
		return err
	}
	fmt.Printf("created: %s《%s》\n", pl.ID, pl.Name)
	return nil
}

func cmdPlaylistDelete(ctx context.Context, id string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTDeletePlaylist(ctx, *server, token, id); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPlaylistAdd(ctx context.Context, id string, refs []string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTAddPlaylistItems(ctx, *server, token, id, refs); err != nil {
		return err
	}
	fmt.Printf("added %d tracks\n", len(refs))
	return nil
}

func cmdPlaylistDelItem(ctx context.Context, id string, ord int) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTDeletePlaylistItem(ctx, *server, token, id, ord); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPlaylistImport(ctx context.Context, what, plName string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	// 判定导入形态：provider:<纯数字id> 或 URL → 外部歌单；其余 → 曲目源物化
	pid, rest, splitErr := client.SplitRef(what)
	isNumericRef := splitErr == nil && isDigits(rest)
	isURL := strings.Contains(what, "://")
	var pl client.Playlist
	switch {
	case isURL:
		pl, err = client.RESTImportPlaylist(ctx, *server, token, "ncm", what, "", plName)
	case isNumericRef:
		pl, err = client.RESTImportPlaylist(ctx, *server, token, pid, rest, "", plName)
	default:
		pl, err = client.RESTImportPlaylist(ctx, *server, token, "", "", what, plName)
	}
	if err != nil {
		return err
	}
	fmt.Printf("imported: %s《%s》 %d 首\n", pl.ID, pl.Name, pl.TrackCount)
	return nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
