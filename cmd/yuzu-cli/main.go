// Command yuzu-cli 是 yuzu-jukebox 的控制端：短命命令行工具。
// 用法：
//
//	yuzu-cli rooms
//	yuzu-cli search <query> [-provider local]
//	yuzu-cli queue <room>
//	yuzu-cli add <room> <track_ref>
//	yuzu-cli skip|pause|resume <room>
//	yuzu-cli seek <room> <seconds>
//	yuzu-cli credential <provider> <payload>
//
// 全局参数：-server -name -password（管理员口令）-room-password（房间访客密码），
// 均可用环境变量 YUZU_SERVER / YUZU_NAME / YUZU_PASSWORD / YUZU_ROOM_PASSWORD 代替。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

var (
	server       = flag.String("server", envOr("YUZU_SERVER", "http://127.0.0.1:8080"), "server base URL")
	name         = flag.String("name", envOr("YUZU_NAME", "cli"), "display name")
	password     = flag.String("password", envOr("YUZU_PASSWORD", ""), "global admin password")
	roomPassword = flag.String("room-password", envOr("YUZU_ROOM_PASSWORD", ""), "room guest password")
	provider     = flag.String("provider", "local", "search provider")
)

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: yuzu-cli <rooms|search|queue|add|skip|pause|resume|seek> [args...]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var err error
	switch args[0] {
	case "rooms":
		err = cmdRooms(ctx)
	case "search":
		if len(args) < 2 {
			err = usageErr("search <query>")
		} else {
			err = cmdSearch(ctx, args[1])
		}
	case "queue":
		if len(args) < 2 {
			err = usageErr("queue <room>")
		} else {
			err = cmdQueue(ctx, args[1])
		}
	case "add":
		if len(args) < 3 {
			err = usageErr("add <room> <track_ref>")
		} else {
			err = wsOp(ctx, args[1], func(cli *client.Client, room string) error {
				return cli.QueueAdd(ctx, room, args[2])
			})
		}
	case "skip", "pause", "resume":
		if len(args) < 2 {
			err = usageErr(args[0] + " <room>")
		} else {
			op := "playback." + args[0]
			err = wsOp(ctx, args[1], func(cli *client.Client, room string) error {
				return cli.PlaybackOp(ctx, op, room, 0)
			})
		}
	case "seek":
		if len(args) < 3 {
			err = usageErr("seek <room> <seconds>")
		} else {
			var sec float64
			sec, err = strconv.ParseFloat(args[2], 64)
			if err == nil {
				err = wsOp(ctx, args[1], func(cli *client.Client, room string) error {
					return cli.PlaybackOp(ctx, "playback.seek", room, int64(sec*1000))
				})
			}
		}
	case "credential":
		if len(args) < 3 {
			err = usageErr("credential <provider> <payload>")
		} else {
			err = cmdCredential(ctx, args[1], args[2])
		}
	default:
		err = usageErr("unknown command: " + args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---------- REST 命令 ----------

func cmdRooms(ctx context.Context) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	rooms, err := client.RESTListRooms(ctx, *server, token)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		fmt.Printf("%-20s %s\n", r.ID, r.Name)
	}
	return nil
}

func cmdSearch(ctx context.Context, query string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	tracks, err := client.RESTSearch(ctx, *server, token, *provider, query)
	if err != nil {
		return err
	}
	for _, t := range tracks {
		fmt.Printf("%-24s %-30s %-20s %ds\n", t.Ref, t.Title, t.Artist, t.DurationMs/1000)
	}
	return nil
}

// ---------- WS 命令 ----------

// connect 校时 + 认证 + 进房。
func connect(ctx context.Context, roomID string) (*client.Client, error) {
	cli, err := client.Dial(ctx, *server)
	if err != nil {
		return nil, err
	}
	if err := cli.ClockSync(ctx, 3); err != nil {
		cli.Close()
		return nil, err
	}
	if _, err := cli.Auth(ctx, *name, *password); err != nil {
		cli.Close()
		return nil, err
	}
	if err := cli.Join(ctx, roomID, *roomPassword); err != nil {
		cli.Close()
		return nil, err
	}
	return cli, nil
}

func wsOp(ctx context.Context, roomID string, op func(*client.Client, string) error) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := op(cli, roomID); err != nil {
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

	var playback *client.Playback
	var queue []client.QueueEntry
	// 收 join 快照三件套
	timeout := time.After(5 * time.Second)
	for playback == nil || queue == nil {
		select {
		case m := <-cli.Events():
			switch m.Type {
			case "playback.changed":
				pb, err := client.ParsePlayback(m)
				if err == nil {
					playback = &pb
				}
			case "queue.changed":
				queue, _ = client.ParseQueue(m)
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for room snapshot")
		}
	}

	if playback.Current != nil {
		fmt.Printf("playing: %s — %s (%dms/%dms)\n",
			playback.Current.Title, playback.Current.Artist,
			playback.ShouldBeMs(cli.ServerNow()), playback.Current.DurationMs)
	} else {
		fmt.Println("playing: (idle)")
	}
	if len(queue) == 0 {
		fmt.Println("queue: (empty)")
	}
	for i, e := range queue {
		fmt.Printf("  %d. [%s] %s — %s (%ds)\n", i, e.EntryID, e.Title, e.Artist, e.DurationMs/1000)
	}
	return nil
}

func cmdCredential(ctx context.Context, providerID, payload string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	return client.RESTSetCredential(ctx, *server, token, providerID, payload)
}

func usageErr(msg string) error { return fmt.Errorf("usage: %s", msg) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
