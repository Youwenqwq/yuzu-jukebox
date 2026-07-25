// Command yuzu-cli 是 yuzu-jukebox 的控制端：短命命令行工具。
//
// 全局参数（可放在子命令前后任意位置，也可用环境变量代替）：
//
//	-server         服务器地址        (YUZU_SERVER, 默认 http://127.0.0.1:8080)
//	-name           显示名            (YUZU_NAME)
//	-password       全局管理员口令     (YUZU_PASSWORD)
//	-room-password  房间访客密码       (YUZU_ROOM_PASSWORD)
//
// 查看子命令帮助：yuzu-cli help <命令> 或 yuzu-cli <命令> --help
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

var (
	server       = flag.String("server", envOr("YUZU_SERVER", "http://127.0.0.1:8080"), "server base URL")
	name         = flag.String("name", envOr("YUZU_NAME", "cli"), "display name")
	password     = flag.String("password", envOr("YUZU_PASSWORD", ""), "global admin password")
	roomPassword = flag.String("room-password", envOr("YUZU_ROOM_PASSWORD", ""), "room guest password")
	provider     = flag.String("provider", "local", "search provider")
	title        = flag.String("title", "", "upload title")
	artist       = flag.String("artist", "", "upload artist")
	durationMs   = flag.Int64("duration-ms", 0, "upload duration in milliseconds (auto-detected if 0)")

	helpWanted bool
)

// ---------- 命令注册表：帮助文本与分发同源 ----------

type command struct {
	usage  string // 用法行（不含程序名）
	desc   string // 一句话描述
	detail string // 详细帮助（多行）
	run    func(args []string) error
}

var commands map[string]command

// 在 init 中填充，避免 commands → errUsage → commands 的包级初始化环。
func init() {
	commands = map[string]command{
	"rooms": {
		usage: "rooms",
		desc:  "列出所有房间",
		detail: `列出服务器上的全部房间（大厅目录）。

需要任意已认证身份。`,
		run: func(args []string) error {
			return withCtx(cmdRooms)
		},
	},
	"search": {
		usage: "search <关键词> [-provider local|ncm]",
		desc:  "搜索曲目",
		detail: `在指定 provider 上搜索曲目，输出 track_ref / 标题 / 艺术家 / 时长。
track_ref 用于 add 点歌。

  -provider   搜索源（默认 local；ncm 需服务器启用对应 provider）

示例：
  yuzu-cli search "海阔天空" -provider ncm`,
		run: func(args []string) error {
			if len(args) < 1 {
				return errUsage("search")
			}
			return withCtx(func(ctx context.Context) error { return cmdSearch(ctx, args[0]) })
		},
	},
	"queue": {
		usage: "queue <room>",
		desc:  "查看房间当前播放与队列",
		detail: `显示房间正在播放的曲目（含实时进度）和等待队列。

需要房间访客密码（-room-password，若房间设有密码）。`,
		run: func(args []string) error {
			if len(args) < 1 {
				return errUsage("queue")
			}
			return withCtx(func(ctx context.Context) error { return cmdQueue(ctx, args[0]) })
		},
	},
	"add": {
		usage: "add <room> <track_ref>",
		desc:  "点歌：把曲目追加到房间队列",
		detail: `把 track_ref 指定的曲目追加到房间队列尾部；房间空闲时自动开播。
track_ref 来自 search 的输出，格式 "<provider>:<id>"，如 ncm:347230。

需要 requester 角色（访客默认拥有）。`,
		run: func(args []string) error {
			if len(args) < 2 {
				return errUsage("add")
			}
			return withCtx(func(ctx context.Context) error { return cmdAdd(ctx, args[0], args[1]) })
		},
	},
	"skip": {
		usage:  "skip <room>",
		desc:   "切歌：结束当前曲目，播放下一首（管理员）",
		detail:  "结束当前曲目（记入播放历史），队列非空时自动播放下一首。\n\n需要 room_admin 角色（-password 携带管理员口令）。",
		run:    playbackCmd("playback.skip"),
	},
	"pause": {
		usage:  "pause <room>",
		desc:   "暂停播放（管理员）",
		detail:  "暂停当前曲目，进度冻结。\n\n需要 room_admin 角色。",
		run:    playbackCmd("playback.pause"),
	},
	"resume": {
		usage:  "resume <room>",
		desc:   "恢复播放（管理员）",
		detail:  "从暂停位置继续播放。\n\n需要 room_admin 角色。",
		run:    playbackCmd("playback.resume"),
	},
	"seek": {
		usage: "seek <room> <秒>",
		desc:  "跳转播放进度（管理员）",
		detail: `把当前曲目跳转到指定秒数，越界自动收敛到 [0, 时长]。

需要 room_admin 角色。

示例：
  yuzu-cli seek lobby 120    # 跳到 2 分钟处`,
		run: func(args []string) error {
			if len(args) < 2 {
				return errUsage("seek")
			}
			sec, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("invalid seconds: %s", args[1])
			}
			return withCtx(func(ctx context.Context) error {
				return cmdPlayback(ctx, "playback.seek", args[0], int64(sec*1000))
			})
		},
	},
	"mkroom": {
		usage: "mkroom <id> <名称>",
		desc:  "创建房间（管理员）",
		detail: `创建一个持久房间。访客密码取 -room-password（不传则无密码）。

需要 room_admin 角色。

示例：
  yuzu-cli mkroom lobby 大厅 -room-password room123`,
		run: func(args []string) error {
			if len(args) < 2 {
				return errUsage("mkroom")
			}
			return withCtx(func(ctx context.Context) error { return cmdMkRoom(ctx, args[0], args[1]) })
		},
	},
	"upload": {
		usage: "upload <文件> [-title t] [-artist a] [-duration-ms n]",
		desc:  "上传本地媒体文件（管理员）",
		detail: `上传音频文件到 local provider。时长自动探测
（WAV 直接解析，其他格式依赖服务器上的 ffprobe），失败时需显式传 -duration-ms。

需要 media_admin 角色。

示例：
  yuzu-cli upload ~/Music/song.wav -title "My Song" -artist "Me"`,
		run: func(args []string) error {
			if len(args) < 1 {
				return errUsage("upload")
			}
			return withCtx(func(ctx context.Context) error { return cmdUpload(ctx, args[0]) })
		},
	},
	"cache": {
		usage: "cache",
		desc:  "查看媒体缓存：已缓存条目、进行中的下载、最近下载历史（管理员）",
		detail: `显示三段缓存状态：
  entries   已落盘的缓存文件（DB 索引）
  downloads 正在进行中的下载（含进度）
  history   最近完成的下载记录（成功/失败及原因，最新在前）

需要 media_admin 角色。`,
		run: func(args []string) error {
			return withCtx(cmdCache)
		},
	},
	"credential": {
		usage: "credential <provider> <payload>",
		desc:  "热更新 provider 凭据（管理员）",
		detail: `提交 provider 凭据（如 ncm 的 MUSIC_U cookie）。服务端先校验
有效性再生效，无需重启。凭据存于服务端，不会下发给客户端。

需要 media_admin 角色。

示例：
  yuzu-cli credential ncm "MUSIC_U=xxxx"`,
		run: func(args []string) error {
			if len(args) < 2 {
				return errUsage("credential")
			}
			return withCtx(func(ctx context.Context) error {
				return cmdCredential(ctx, args[0], args[1])
			})
		},
	},
	"qrlogin": {
		usage: "qrlogin <provider>",
		desc:  "扫码登录 provider（管理员）",
		detail: `在终端渲染二维码，用对应 App（如网易云音乐）扫码并确认。
凭据由服务端在扫码成功后自动提取、校验并热生效，不经过本机。

需要 media_admin 角色。二维码约 2 分钟过期，全程最多等待 5 分钟。

示例：
  yuzu-cli qrlogin ncm`,
		run: func(args []string) error {
			if len(args) < 1 {
				return errUsage("qrlogin")
			}
			return cmdQRLogin(args[0])
		},
	},
	}
}

// ---------- main ----------

func main() {
	args := parseFlagsAnywhere()
	if helpWanted {
		printHelp(args)
		os.Exit(0)
	}
	if len(args) == 0 {
		printHelp(nil)
		os.Exit(2)
	}
	if args[0] == "help" {
		printHelp(args[1:])
		os.Exit(0)
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command: %s (see 'yuzu-cli help')\n", args[0])
		os.Exit(2)
	}
	if err := cmd.run(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parseFlagsAnywhere 允许 flag 出现在子命令前后任意位置。
// 我们所有 flag 都是带值类型，逐个扫描即可安全分离；-h/--help 单独拦截。
func parseFlagsAnywhere() []string {
	var positional, flagArgs []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help" || a == "-help":
			helpWanted = true
		case strings.HasPrefix(a, "-"):
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	return positional
}

// ---------- 帮助 ----------

func printHelp(args []string) {
	if len(args) > 0 {
		if cmd, ok := commands[args[0]]; ok {
			fmt.Printf("用法: yuzu-cli %s\n\n%s\n%s", cmd.usage, cmd.desc, cmd.detail)
			if !strings.HasSuffix(cmd.detail, "\n") {
				fmt.Println()
			}
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
	}
	fmt.Println(`yuzu-cli — yuzu-jukebox 控制端

用法: yuzu-cli <命令> [参数...] [全局 flag]

命令:`)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := commands[n]
		fmt.Printf("  %-28s %s\n", c.usage, c.desc)
	}
	fmt.Println(`
全局 flag（可放任意位置，括号内为对应环境变量）:
  -server          服务器地址 (YUZU_SERVER, 默认 http://127.0.0.1:8080)
  -name            显示名 (YUZU_NAME)
  -password        全局管理员口令 (YUZU_PASSWORD)
  -room-password   房间访客密码 (YUZU_ROOM_PASSWORD)

查看单个命令的详细帮助: yuzu-cli help <命令>`)
}

func errUsage(cmdName string) error {
	return fmt.Errorf("usage: yuzu-cli %s", commands[cmdName].usage)
}

// ---------- 命令实现 ----------

func withCtx(f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return f(ctx)
}

// playbackCmd 生成 skip/pause/resume 这类单房间操作的 run 函数。
func playbackCmd(op string) func(args []string) error {
	return func(args []string) error {
		if len(args) < 1 {
			// 从 op 反查命令名
			for n, c := range commands {
				if c.usage == strings.TrimPrefix(op, "playback.")+" <room>" {
					return errUsage(n)
				}
			}
			return fmt.Errorf("usage: yuzu-cli %s <room>", op)
		}
		return withCtx(func(ctx context.Context) error {
			return cmdPlayback(ctx, op, args[0], 0)
		})
	}
}

func cmdRooms(ctx context.Context) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	rooms, err := client.RESTListRooms(ctx, *server, token)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		fmt.Println("(no rooms — create one with: yuzu-cli mkroom <id> <name>)")
		return nil
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
	if len(tracks) == 0 {
		fmt.Println("(no results)")
		return nil
	}
	for _, t := range tracks {
		fmt.Printf("%-24s %-30s %-20s %ds\n", t.Ref, t.Title, t.Artist, t.DurationMs/1000)
	}
	return nil
}

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

func cmdAdd(ctx context.Context, roomID, trackRef string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.QueueAdd(ctx, roomID, trackRef); err != nil {
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
		if playback.Current.StreamURL != "" {
			fmt.Printf("stream:  %s\n", playback.Current.StreamURL)
		}
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

func cmdMkRoom(ctx context.Context, id, roomName string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	if err := client.RESTCreateRoom(ctx, *server, token, id, roomName, *roomPassword); err != nil {
		return err
	}
	fmt.Printf("room %q created (guest password: %s)\n", id, orNone(*roomPassword))
	return nil
}

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

func cmdCredential(ctx context.Context, providerID, payload string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	return client.RESTSetCredential(ctx, *server, token, providerID, payload)
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
	case n < 1 << 10:
		return fmt.Sprintf("%dB", n)
	case n < 1 << 20:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	}
}

// cmdQRLogin 二维码登录：渲染二维码并轮询，凭据由服务端在扫码成功后自动生效。
// 轮询不套全局 15s 超时（扫码是分钟级操作），单独给 5 分钟。
func cmdQRLogin(providerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	sess, err := client.RESTQRLoginStart(ctx, *server, token, providerID)
	if err != nil {
		return err
	}
	fmt.Println("请用网易云音乐 App 扫描以下二维码：")
	qrterminal.GenerateHalfBlock(sess.QRContent, qrterminal.L, os.Stdout)

	lastStatus := ""
	for {
		res, err := client.RESTQRLoginPoll(ctx, *server, token, providerID, sess.Key)
		if err != nil {
			return err
		}
		if res.Status != lastStatus {
			switch res.Status {
			case "waiting":
				fmt.Println("等待扫码…")
			case "scanned":
				fmt.Println("已扫码，请在 App 上确认登录…")
			}
			lastStatus = res.Status
		}
		switch res.Status {
		case "ok":
			fmt.Println("✓", res.Message)
			return nil
		case "expired":
			return fmt.Errorf("二维码已过期，请重新运行")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待超时")
		case <-time.After(2 * time.Second):
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
