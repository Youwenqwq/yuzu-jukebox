// ops.go — 队列操作与播放控制命令实现。
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

// connect 校时 + 认证 + 进房。认证优先用缓存的 OIDC 会话，否则 guest。
func connect(ctx context.Context, roomID string) (*client.Client, error) {
	cli, err := client.Dial(ctx, *server)
	if err != nil {
		return nil, err
	}
	if err := cli.ClockSync(ctx, 3); err != nil {
		cli.Close()
		return nil, err
	}
	if s := loadSession(*server); s != nil {
		if _, err := cli.AuthToken(ctx, s.Token); err != nil {
			cli.Close()
			return nil, fmt.Errorf("缓存会话已失效，请重新 yuzu-cli login: %w", err)
		}
	} else if _, err := cli.Auth(ctx, *name, *password); err != nil {
		cli.Close()
		return nil, err
	}
	if err := cli.Join(ctx, roomID, *roomPassword); err != nil {
		cli.Close()
		return nil, err
	}
	return cli, nil
}

func cmdSearch(ctx context.Context, query string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	tracks, err := client.RESTSearch(ctx, *server, token, *provider, query)
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		fmt.Println("(no results)")
		return nil
	}
	for _, t := range tracks {
		fmt.Printf("%-24s %-30s %-20s %ds\n", t.Ref, t.Title, t.Artist, t.DurationMs/1000)
	}
	return nil
}

func cmdAdd(ctx context.Context, roomID string, trackRefs []string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if len(trackRefs) == 1 {
		if err := cli.QueueAdd(ctx, roomID, trackRefs[0]); err != nil {
			return err
		}
	} else if _, err := cli.QueueAddMany(ctx, roomID, trackRefs); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPlayback(ctx context.Context, op, roomID string, positionMs int64) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.PlaybackOp(ctx, op, roomID, positionMs); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdQueueDel(ctx context.Context, roomID, entryID string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.QueueRemove(ctx, roomID, entryID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdQueueMove(ctx context.Context, roomID, entryID string, to int) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.QueueMove(ctx, roomID, entryID, to); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdQueue(ctx context.Context, roomID string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	snap, err := cli.AwaitSnapshot(5 * time.Second)
	if err != nil {
		return err
	}
	if snap.Playback.Current != nil {
		cur := snap.Playback.Current
		state := "playing"
		if !snap.Playback.Playing {
			state = "paused "
		}
		if pending := snap.Playback.PendingStartMs(cli.ServerNow()); pending > 0 {
			state = fmt.Sprintf("starting in %dms", pending)
		}
		fmt.Printf("%s: %s — %s (%dms/%dms)\n", state, cur.Title, cur.Artist,
			snap.Playback.DisplayPositionMs(cli.ServerNow()), cur.DurationMs)
		if cur.StreamURL != "" {
			fmt.Printf("stream:  %s\n", cur.StreamURL)
		}
	} else {
		fmt.Println("playing: (idle)")
	}
	if snap.Radio != nil {
		fmt.Printf("radio:   %s (%s)\n", snap.Radio.Source, radioFlags(snap.Radio))
	}
	if len(snap.Queue) == 0 {
		fmt.Println("queue: (empty)")
	}
	for i, e := range snap.Queue {
		fmt.Printf("  %d. [%s] %s — %s (%ds)\n", i, e.EntryID, e.Title, e.Artist, e.DurationMs/1000)
	}
	return nil
}

// radioFlags 电台标志的人性化描述。
func radioFlags(r *client.RadioInfo) string {
	if !r.Finite {
		return "无限流"
	}
	flags := ""
	if r.Shuffle {
		flags += "shuffle "
	}
	if r.Once {
		flags += "once "
	}
	if flags == "" {
		return "顺序循环"
	}
	return flags[:len(flags)-1]
}

func cmdStatus(ctx context.Context, roomID string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	snap, err := cli.AwaitSnapshot(5 * time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("room:      %s\n", roomID)
	if snap.Playback.Current != nil {
		cur := snap.Playback.Current
		state := "playing"
		if !snap.Playback.Playing {
			state = "paused"
		}
		if pending := snap.Playback.PendingStartMs(cli.ServerNow()); pending > 0 {
			state = fmt.Sprintf("starting in %dms", pending)
		}
		fmt.Printf("state:     %s %s — %s (%ds/%ds)\n", state, cur.Title, cur.Artist,
			snap.Playback.DisplayPositionMs(cli.ServerNow())/1000, cur.DurationMs/1000)
		if cur.Album != "" {
			fmt.Printf("album:     %s\n", cur.Album)
		}
		if cur.BitrateKbps > 0 || cur.SizeBytes > 0 {
			fmt.Printf("quality:   %d kbps, %s\n", cur.BitrateKbps, humanBytes(cur.SizeBytes))
		}
	} else {
		fmt.Println("state:     idle")
	}
	if snap.Radio != nil {
		fmt.Printf("radio:     %s\n", snap.Radio.Source)
		fmt.Printf("           %s (%s)\n", snap.Radio.Description, radioFlags(snap.Radio))
	} else {
		fmt.Println("radio:     (未绑定，队列播完即停)")
	}
	fmt.Printf("queue:     %d 首待播\n", len(snap.Queue))
	names := make([]string, 0, len(snap.Listeners))
	for _, l := range snap.Listeners {
		names = append(names, l.Name)
	}
	fmt.Printf("listeners: %d 人 (%s)\n", len(snap.Listeners), strings.Join(names, ", "))
	return nil
}
