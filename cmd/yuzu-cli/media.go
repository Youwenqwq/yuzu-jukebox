// media.go — 媒体上传与缓存命令实现。
package main

import (
	"context"
	"fmt"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdUpload(ctx context.Context, filePath string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	track, err := client.RESTUpload(ctx, *server, token, filePath, *title, *artist, *durationMs)
	if err != nil {
		return err
	}
	fmt.Printf("uploaded: %s — %s (%ds) ref=%s\n", track.Title, track.Artist, track.DurationMs/1000, track.Ref)
	return nil
}

func cmdCache(ctx context.Context) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	view, err := client.RESTCacheView(ctx, *server, token)
	if err != nil {
		return err
	}
	fmt.Printf("== downloading (%d) ==\n", len(view.Downloads))
	for _, d := range view.Downloads {
		fmt.Printf("  %-28s %s / %s\n", d.TrackRef, humanBytes(d.Fetched), humanBytes(d.Total))
	}
	fmt.Printf("== cached (%d) ==\n", len(view.Entries))
	for _, e := range view.Entries {
		fmt.Printf("  %-28s %s\n", e.TrackRef, humanBytes(e.SizeBytes))
	}
	fmt.Printf("== history (%d) ==\n", len(view.History))
	for _, h := range view.History {
		line := fmt.Sprintf("  %-28s %-8s %s", h.TrackRef, h.Status, humanBytes(h.Fetched))
		if h.Error != "" {
			line += "  err: " + h.Error
		}
		fmt.Println(line)
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n < 0:
		return "?"
	case n < 1<<10:
		return fmt.Sprintf("%dB", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	}
}
