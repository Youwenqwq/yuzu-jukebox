// Command yuzu-cli 是 yuzu-jukebox 的控制端：短命命令行工具。
//
// 全局参数（可放在子命令前后任意位置，也可用环境变量代替）：
//
//	-server           服务器地址       (YUZU_SERVER, 默认 http://127.0.0.1:8080)
//	-name             显示名           (YUZU_NAME)
//	-password         全局管理员口令    (YUZU_PASSWORD)
//	-room-password    房间访问凭据      (YUZU_ROOM_PASSWORD)
//	-room-access      建房访问模式
//	-room-code-period 动态码轮换周期
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
)

var (
	server         = flag.String("server", envOr("YUZU_SERVER", "http://127.0.0.1:8080"), "server base URL")
	name           = flag.String("name", envOr("YUZU_NAME", "cli"), "display name")
	password       = flag.String("password", envOr("YUZU_PASSWORD", ""), "global admin password")
	roomPassword   = flag.String("room-password", envOr("YUZU_ROOM_PASSWORD", ""), "room access credential or static password")
	roomAccessMode = flag.String("room-access", "", "room guest access mode: open, static_password, rotating_code")
	roomCodePeriod = flag.Duration("room-code-period", 24*time.Hour, "rotating room code period")
	provider       = flag.String("provider", "local", "search provider")
	title          = flag.String("title", "", "upload title")
	artist         = flag.String("artist", "", "upload artist")
	durationMs     = flag.Int64("duration-ms", 0, "upload duration in milliseconds (auto-detected if 0)")
	shuffle        = flag.Bool("shuffle", false, "radio: shuffle mode")
	once           = flag.Bool("once", false, "radio: play through once, no loop")
	limit          = flag.Int("limit", 50, "pagination limit")

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
		"integration list": {
			usage:  "integration list",
			desc:   "列出已配置的 Integration",
			detail: "列出所有已配置的 Integration；输出不包含 Integration token。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				return withCtx(cmdIntegrations)
			},
		},
		"integration create": {
			usage:  "integration create <id> <name>",
			desc:   "创建 Integration 并签发 token",
			detail: "创建持久 Integration；token 只在本次响应中显示，请立即保存。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("integration create")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationCreate(ctx, args[0], args[1])
				})
			},
		},
		"integration rename": {
			usage:  "integration rename <id> <name>",
			desc:   "修改 Integration 名称",
			detail: "修改 Integration 的显示名称，不影响 ID 或 token。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("integration rename")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationRename(ctx, args[0], args[1])
				})
			},
		},
		"integration enable": {
			usage:  "integration enable <id>",
			desc:   "启用 Integration",
			detail: "恢复 Integration token 的 actor resolve 权限。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("integration enable")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationSetActive(ctx, args[0], true)
				})
			},
		},
		"integration disable": {
			usage:  "integration disable <id>",
			desc:   "停用 Integration",
			detail: "立即拒绝 Integration token 并吊销由其签发的 actor sessions。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("integration disable")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationSetActive(ctx, args[0], false)
				})
			},
		},
		"integration rotate-token": {
			usage:  "integration rotate-token <id>",
			desc:   "轮换 Integration token",
			detail: "旧 token 与已有 actor sessions 立即失效；新 token 只显示一次。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("integration rotate-token")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationRotateToken(ctx, args[0])
				})
			},
		},
		"integration delete": {
			usage:  "integration delete <id>",
			desc:   "删除 Integration",
			detail: "删除 Integration、actor sessions、scope 与 subject 绑定；不会删除 Principal 或 Room grants。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("integration delete")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationDelete(ctx, args[0])
				})
			},
		},
		"integration scope list": {
			usage:  "integration scope list <integration_id>",
			desc:   "列出 Integration 的 external scope 绑定",
			detail: "输出 adapter_id、scope_type、scope_id 与默认 room_id。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("integration scope list")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationScopes(ctx, args[0])
				})
			},
		},
		"integration scope bind": {
			usage:  "integration scope bind <integration_id> <adapter_id> <scope_type> <scope_id> <room_id>",
			desc:   "绑定 external scope 的默认 Room",
			detail: "创建或更新 Integration external scope 到默认 Room 的绑定。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 5 {
					return errUsage("integration scope bind")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationScopeBind(ctx, args[0], args[1], args[2], args[3], args[4])
				})
			},
		},
		"integration scope unbind": {
			usage:  "integration scope unbind <integration_id> <adapter_id> <scope_type> <scope_id> <room_id>",
			desc:   "解除 external scope 的默认 Room 绑定",
			detail: "解除指定 Integration external scope 与默认 Room 的绑定。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 5 {
					return errUsage("integration scope unbind")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationScopeUnbind(ctx, args[0], args[1], args[2], args[3], args[4])
				})
			},
		},
		"integration subject list": {
			usage:  "integration subject list <integration_id>",
			desc:   "列出 Integration 的 external subject 链接",
			detail: "输出 adapter_id、scope、subject_id 与关联的 principal_id。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("integration subject list")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationSubjects(ctx, args[0])
				})
			},
		},
		"integration subject link": {
			usage:  "integration subject link <integration_id> <adapter_id> <scope_type> <scope_id> <subject_id> <principal_id>",
			desc:   "关联 external subject 与 Principal",
			detail: "创建或更新 external subject 到持久 Principal 的关联。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 6 {
					return errUsage("integration subject link")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationSubjectLink(ctx, args[0], args[1], args[2], args[3], args[4], args[5])
				})
			},
		},
		"integration subject unlink": {
			usage:  "integration subject unlink <integration_id> <adapter_id> <scope_type> <scope_id> <subject_id> <principal_id>",
			desc:   "解除 external subject 与 Principal 的关联",
			detail: "解除指定 external subject 与 Principal 的关联。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 6 {
					return errUsage("integration subject unlink")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdIntegrationSubjectUnlink(ctx, args[0], args[1], args[2], args[3], args[4], args[5])
				})
			},
		},
		"principal list": {
			usage:  "principal list [query] [-limit 50]",
			desc:   "列出或搜索 Principal",
			detail: "按 ID 或名称搜索 Principal；不传 query 时列出 Principal。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				query := ""
				if len(args) > 0 {
					query = args[0]
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPrincipals(ctx, query)
				})
			},
		},
		"room controller list": {
			usage:  "room controller list <room>",
			desc:   "列出 Room controller grants",
			detail: "列出指定 Room 的显式 controller grants。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("room controller list")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdRoomControllers(ctx, args[0])
				})
			},
		},
		"room controller grant": {
			usage:  "room controller grant <room> <principal_id>",
			desc:   "授予 Principal 指定 Room 的 controller capability",
			detail: "为 Principal 授予指定 Room 的 controller capability。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("room controller grant")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdRoomControllerGrant(ctx, args[0], args[1])
				})
			},
		},
		"room controller revoke": {
			usage:  "room controller revoke <room> <principal_id>",
			desc:   "撤销 Principal 指定 Room 的 controller capability",
			detail: "撤销 Principal 在指定 Room 的 controller capability。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("room controller revoke")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdRoomControllerRevoke(ctx, args[0], args[1])
				})
			},
		},
		"room list": {
			usage: "room list",
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
			usage: "add <room> <track_ref> [<track_ref>...]",
			desc:  "点歌：把曲目追加到房间队列（多首原子批量入队）",
			detail: `把 track_ref 指定的曲目追加到房间队列尾部；房间空闲时自动开播。
track_ref 来自 search 的输出，格式 "<provider>:<id>"，如 ncm:347230。

给定多个 track_ref 时走原子批量入队：整体校验，任一失败一条不加。

需要 requester 角色（访客默认拥有）。`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("add")
				}
				return withCtx(func(ctx context.Context) error { return cmdAdd(ctx, args[0], args[1:]) })
			},
		},
		"skip": {
			usage:  "skip <room>",
			desc:   "切歌：结束当前曲目，播放下一首（Room controller）",
			detail: "结束当前曲目（记入播放历史），队列非空时自动播放下一首。\n\n需要该 Room controller（全局 room_admin 或 Room grant）。",
			run:    playbackCmd("playback.skip"),
		},
		"pause": {
			usage:  "pause <room>",
			desc:   "暂停播放（Room controller）",
			detail: "暂停当前曲目，进度冻结。\n\n需要该 Room controller（全局 room_admin 或 Room grant）。",
			run:    playbackCmd("playback.pause"),
		},
		"resume": {
			usage:  "resume <room>",
			desc:   "恢复播放（Room controller）",
			detail: "从暂停位置继续播放。\n\n需要该 Room controller（全局 room_admin 或 Room grant）。",
			run:    playbackCmd("playback.resume"),
		},
		"seek": {
			usage: "seek <room> <秒>",
			desc:  "跳转播放进度（Room controller）",
			detail: `把当前曲目跳转到指定秒数，越界自动收敛到 [0, 时长]。

需要该 Room controller（全局 room_admin 或 Room grant）。

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
		"room create": {
			usage: "room create <id> <名称>",
			desc:  "创建房间（管理员）",
			detail: `创建一个持久房间。-room-access 可选 open、static_password、
rotating_code；动态码轮换周期取 -room-code-period，默认 24h。静态密码取
-room-password。不传 -room-access 时保持兼容：有密码为 static_password，
否则为 open。

需要 room_admin 角色。

示例：
  yuzu-cli room create lobby 大厅 -room-access rotating_code -room-code-period 24h`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("room create")
				}
				return withCtx(func(ctx context.Context) error { return cmdMkRoom(ctx, args[0], args[1]) })
			},
		},
		"room access": {
			usage: "room access <id> <open|static_password|rotating_code>",
			desc:  "修改房间访问模式（管理员）",
			detail: `热更新房间访问模式。static_password 从 -room-password 读取新密码；
rotating_code 从 -room-code-period 读取轮换周期，默认 24h。

需要 room_admin 角色。`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("room access")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdRoomAccess(ctx, args[0], args[1])
				})
			},
		},
		"room code": {
			usage:  "room code <id>",
			desc:   "查看当前房间动态验证码",
			detail: "显示当前动态验证码及过期时间。CLI 认证需要 room_admin；外部 Integration 应使用 actor token 直接调用对应 REST API。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("room code")
				}
				return withCtx(func(ctx context.Context) error { return cmdRoomCode(ctx, args[0]) })
			},
		},
		"media upload": {
			usage: "media upload <文件> [-title t] [-artist a] [-duration-ms n]",
			desc:  "上传本地媒体文件（管理员）",
			detail: `上传音频文件到 local provider。时长自动探测
（WAV 直接解析，其他格式依赖服务器上的 ffprobe），失败时需显式传 -duration-ms。

需要 media_admin 角色。

示例：
  yuzu-cli upload ~/Music/song.wav -title "My Song" -artist "Me"`,
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("media upload")
				}
				return withCtx(func(ctx context.Context) error { return cmdUpload(ctx, args[0]) })
			},
		},
		"media cache": {
			usage: "media cache",
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
		"provider credential": {
			usage: "provider credential <provider> <payload>",
			desc:  "热更新 provider 凭据（管理员）",
			detail: `提交 provider 凭据（如 ncm 的 MUSIC_U cookie）。服务端先校验
有效性再生效，无需重启。凭据存于服务端，不会下发给客户端。

需要 media_admin 角色。

示例：
  yuzu-cli credential ncm "MUSIC_U=xxxx"`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("provider credential")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdCredential(ctx, args[0], args[1])
				})
			},
		},
		"provider list": {
			usage: "provider list",
			desc:  "列出已注册的 Provider 及凭据状态",
			detail: `显示服务器上注册的全部 Provider；支持凭据的 Provider 附带
凭据健康状态（unset / ok / invalid）。状态由服务端定期探活维护。`,
			run: func(args []string) error {
				return withCtx(cmdProviders)
			},
		},
		"playlist list": {
			usage:  "playlist list",
			desc:   "列出全部歌单",
			detail: "列出服务器上的歌单（含曲目数）。查看歌单内容用 playlist show。",
			run: func(args []string) error {
				return withCtx(cmdPlaylists)
			},
		},
		"playlist show": {
			usage: "playlist show <id> [offset] [-limit 50]",
			desc:  "查看歌单内容（分页）",
			detail: `分页显示歌单条目（序号、track_ref、标题、艺术家、时长）。

示例：
  yuzu-cli playlist show pl_a1b2c3 0 -limit 20`,
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("playlist show")
				}
				offset := 0
				if len(args) > 1 {
					offset, _ = strconv.Atoi(args[1])
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistShow(ctx, args[0], offset)
				})
			},
		},
		"playlist create": {
			usage:  "playlist create <名称> [描述]",
			desc:   "创建歌单（管理员）",
			detail: "创建一张空的通用歌单。\n\n需要 media_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("playlist create")
				}
				desc := ""
				if len(args) > 1 {
					desc = args[1]
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistCreate(ctx, args[0], desc)
				})
			},
		},
		"playlist delete": {
			usage:  "playlist delete <id>",
			desc:   "删除歌单（管理员）",
			detail: "删除歌单及其全部条目。\n\n需要 media_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("playlist delete")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistDelete(ctx, args[0])
				})
			},
		},
		"playlist add": {
			usage: "playlist add <id> <track_ref>...",
			desc:  "向歌单追加曲目（管理员）",
			detail: `把一个或多个 track_ref 追加到歌单尾部（单次最多 100 首）。
元数据快照自各 provider 实时获取。

需要 media_admin 角色。`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("playlist add")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistAdd(ctx, args[0], args[1:])
				})
			},
		},
		"playlist delitem": {
			usage:  "playlist delitem <id> <ord>",
			desc:   "删除歌单中的指定条目（管理员）",
			detail: "按序号（ord，playlist show 输出第一列）删除条目，后续序号自动重排。\n\n需要 media_admin 角色。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("playlist delitem")
				}
				ord, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("invalid ord: %s", args[1])
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistDelItem(ctx, args[0], ord)
				})
			},
		},
		"playlist move": {
			usage:  "playlist move <id> <ord> <to_ord>",
			desc:   "移动歌单条目到指定位置（管理员）",
			detail: "把序号 ord 的条目移动到 to_ord（超出范围自动 clamp 到 [1, 歌单长度]），其余序号自动重排。\n\n需要 media_admin 角色。",
			run: func(args []string) error {
				if len(args) < 3 {
					return errUsage("playlist move")
				}
				ord, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("invalid ord: %s", args[1])
				}
				toOrd, err := strconv.Atoi(args[2])
				if err != nil {
					return fmt.Errorf("invalid to_ord: %s", args[2])
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistMove(ctx, args[0], ord, toOrd)
				})
			},
		},
		"playlist import": {
			usage: "playlist import <ncm:歌单id|URL|ncm:daily> [名称]",
			desc:  "导入外部歌单或曲目源快照（管理员）",
			detail: `两种用法：
  playlist import ncm:24381616     导入 ncm 歌单（也接受完整 URL）
  playlist import ncm:daily        把每日推荐物化成歌单

需要 media_admin 角色。长歌单分页拉取，可能需要几秒。`,
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("playlist import")
				}
				name := ""
				if len(args) > 1 {
					name = args[1]
				}
				return withCtx(func(ctx context.Context) error {
					return cmdPlaylistImport(ctx, args[0], name)
				})
			},
		},
		"radio play": {
			usage: "radio play <room> <source> [-shuffle] [-once]",
			desc:  "房间进入电台模式：绑定曲目源自动续播（Room controller）",
			detail: `让房间绑定一个曲目源，队列见底时自动批量补充，实现无人值守续播。

source 取值：
  playlist:<id>       通用歌单（playlist show 可查 id）
  ncm:daily            网易云每日推荐
  ncm:fm               网易云私人FM（无限流，不接受 -shuffle/-once）
  ncm:simi:<song_id>  相似歌曲电台（种子=当前播放曲目）
  ncm:heart:<song_id> 心动模式（种子=我喜欢+当前播放）

-shuffle 洗牌袋随机（仅有限源）；-once 播完即停（仅有限源）。
需要该 Room controller（全局 room_admin 或 Room grant）。

示例：
  yuzu-cli radio play lobby playlist:pl_a1b2c3 -shuffle
  yuzu-cli radio play lobby ncm:fm`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("radio play")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdRadioPlay(ctx, args[0], args[1])
				})
			},
		},
		"radio stop": {
			usage:  "radio stop <room>",
			desc:   "退出电台模式（Room controller）",
			detail: "解绑曲目源；队列中已有的曲目继续播放。\n\n需要该 Room controller（全局 room_admin 或 Room grant）。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("radio stop")
				}
				return withCtx(func(ctx context.Context) error {
					return cmdRadioStop(ctx, args[0])
				})
			},
		},
		"status": {
			usage: "status <room>",
			desc:  "房间总览：播放状态、电台绑定、队列规模、听众",
			detail: `一眼看清房间当前状态：正在放什么、是否绑定曲目源（电台）、
队列里还压着几首、谁在线。想看队列明细用 queue。`,
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("status")
				}
				return withCtx(func(ctx context.Context) error { return cmdStatus(ctx, args[0]) })
			},
		},
		"login": {
			usage: "login",
			desc:  "OIDC 登录（设备授权流，会话缓存到本地）",
			detail: `从服务端发现 OIDC 配置，启动设备授权流：终端显示验证链接和用户码，
在手机/电脑浏览器中确认后自动完成登录。会话缓存在
~/.config/yuzu-cli/session.json，之后所有命令自动携带该身份。

可用环境变量覆盖服务端发现：YUZU_OIDC_ISSUER / YUZU_OIDC_CLIENT_ID。`,
			run: func(args []string) error {
				return withCtx(func(ctx context.Context) error { return cmdLogin(ctx) })
			},
		},
		"logout": {
			usage: "logout",
			desc:  "登出：清除本地缓存的登录会话",
			run:   func(args []string) error { return cmdLogout() },
		},
		"whoami": {
			usage: "whoami",
			desc:  "查看当前生效身份（缓存会话或 guest）",
			run: func(args []string) error {
				return withCtx(func(ctx context.Context) error { return cmdWhoami(ctx) })
			},
		},
		"identity bind-code": {
			usage:  "identity bind-code",
			desc:   "为当前 OIDC 身份签发 external subject 绑定码",
			detail: "签发一个 10 分钟有效的一次性绑定码，供用户复制到目标 IM。\n\n只使用当前 server 对应的缓存 OIDC session；未登录时请先运行 yuzu-cli login。",
			run: func(args []string) error {
				return withCtx(cmdIdentityBindCode)
			},
		},
		"player list": {
			usage:  "player list",
			desc:   "列出持久 Player 资源及在线状态（管理员）",
			detail: "显示全部 Player：ID、名称、启停状态、Room 分配，以及在线设备状态。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				return withCtx(func(ctx context.Context) error { return cmdPlayers(ctx) })
			},
		},
		"player show": {
			usage: "player show <player_id>",
			desc:  "查看 Player 资源（管理员）",
			run: func(args []string) error {
				if len(args) != 1 {
					return errUsage("player show")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerShow(ctx, args[0]) })
			},
		},
		"player create": {
			usage:  "player create <player_id> <name>",
			desc:   "创建 Player 并签发一次性 key（管理员）",
			detail: "key 仅在创建响应中显示一次；保存后配置给 yuzu-agent -key。",
			run: func(args []string) error {
				if len(args) != 2 {
					return errUsage("player create")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerCreate(ctx, args[0], args[1]) })
			},
		},
		"player rename": {
			usage: "player rename <player_id> <name>",
			desc:  "重命名 Player（管理员）",
			run: func(args []string) error {
				if len(args) != 2 {
					return errUsage("player rename")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerRename(ctx, args[0], args[1]) })
			},
		},
		"player enable": {
			usage: "player enable <player_id>",
			desc:  "启用 Player（管理员）",
			run: func(args []string) error {
				if len(args) != 1 {
					return errUsage("player enable")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerSetActive(ctx, args[0], true) })
			},
		},
		"player disable": {
			usage:  "player disable <player_id>",
			desc:   "停用 Player 并立即断开在线 Agent（管理员）",
			detail: "Room 分配保留；重新启用后，Agent 可使用现有 key 重连。",
			run: func(args []string) error {
				if len(args) != 1 {
					return errUsage("player disable")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerSetActive(ctx, args[0], false) })
			},
		},
		"player key rotate": {
			usage:  "player key rotate <player_id>",
			desc:   "轮换 Player key 并断开旧连接（管理员）",
			detail: "新 key 仅显示一次；旧 key 立即失效。",
			run: func(args []string) error {
				if len(args) != 1 {
					return errUsage("player key rotate")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerRotateKey(ctx, args[0]) })
			},
		},
		"player delete": {
			usage:  "player delete <player_id>",
			desc:   "删除 Player、Room 分配并断开 Agent（管理员）",
			detail: "删除不可撤销；Player key 同时失效。",
			run: func(args []string) error {
				if len(args) != 1 {
					return errUsage("player delete")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerDelete(ctx, args[0]) })
			},
		},
		"player volume": {
			usage:  "player volume <player_id> <0-100>",
			desc:   "远程设置播放端音量（管理员）",
			detail: "向指定播放端下发音量指令，立即生效。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("player volume")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerVolume(ctx, args[0], args[1]) })
			},
		},
		"player mute": {
			usage: "player mute <player_id> on|off",
			desc:  "远程静音/取消静音播放端（管理员）",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("player mute")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerMute(ctx, args[0], args[1]) })
			},
		},
		"player bind": {
			usage: "player bind <player_id> <room>",
			desc:  "将 headless player 分配到 Room（管理员）",
			detail: `持久分配指定 Player；在线时立即迁入目标 Room，离线时等待重连。
Agent 重连后会按分配自动进入 Room；一个 Room 可包含多个 Player。`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("player bind")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerBind(ctx, args[0], args[1]) })
			},
		},
		"player unbind": {
			usage:  "player unbind <player_id> <room>",
			desc:   "解除 headless player 的 Room 分配（管理员）",
			detail: "解除持久分配；在线 Agent 立即离开 Room，但保持连接等待重新分配。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("player unbind")
				}
				return withCtx(func(ctx context.Context) error { return cmdPlayerUnbind(ctx, args[0], args[1]) })
			},
		},
		"room players": {
			usage:  "room players <room>",
			desc:   "列出 Room 的 headless players（管理员）",
			detail: "显示持久分配与当前在线 player 的并集，包括设备实际音量。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("room players")
				}
				return withCtx(func(ctx context.Context) error { return cmdRoomPlayers(ctx, args[0]) })
			},
		},
		"room volume": {
			usage:  "room volume <room> <0-100>",
			desc:   "设置 Room headless output 全局音量",
			detail: "持久化 desired volume，并向当前 Room 的全部在线 headless Agent 下发；离线 Agent 重连后自动收敛。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("room volume")
				}
				return withCtx(func(ctx context.Context) error { return cmdRoomVolume(ctx, args[0], args[1]) })
			},
		},
		"queue del": {
			usage:  "queue del <room> <entry_id>",
			desc:   "移除队列条目（本人或 Room controller）",
			detail: "按 entry_id 移除队列条目。普通用户只能移除自己点的；该 Room controller（全局 room_admin 或 Room grant）可移除他人点的。\n\nentry_id 见 queue 输出第一列。",
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("queue del")
				}
				return withCtx(func(ctx context.Context) error { return cmdQueueDel(ctx, args[0], args[1]) })
			},
		},
		"queue move": {
			usage:  "queue move <room> <entry_id> <位置>",
			desc:   "移动队列条目到指定位置（Room controller）",
			detail: "把队列条目移动到目标序号（0 起）。\n\n需要该 Room controller（全局 room_admin 或 Room grant）。",
			run: func(args []string) error {
				if len(args) < 3 {
					return errUsage("queue move")
				}
				to, err := strconv.Atoi(args[2])
				if err != nil {
					return fmt.Errorf("位置必须是数字")
				}
				return withCtx(func(ctx context.Context) error { return cmdQueueMove(ctx, args[0], args[1], to) })
			},
		},
		"room delete": {
			usage:  "room delete <id>",
			desc:   "删除房间（管理员）",
			detail: "删除房间并级联清理其队列与播放历史。\n\n需要 room_admin 角色。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("room delete")
				}
				return withCtx(func(ctx context.Context) error { return cmdRoomDelete(ctx, args[0]) })
			},
		},
		"room history": {
			usage:  "room history <room> [offset] [-limit 20]",
			desc:   "查看房间播放历史（最新在前）",
			detail: "显示最近播放记录：开始时间、标题、点歌人、结束原因\n（natural / skipped / seek-past-end）。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("room history")
				}
				offset := 0
				if len(args) > 1 {
					offset, _ = strconv.Atoi(args[1])
				}
				return withCtx(func(ctx context.Context) error { return cmdHistory(ctx, args[0], offset) })
			},
		},
		"room top": {
			usage:  "room top <room> [-limit 20]",
			desc:   "房间曲目热度榜：播放次数、首播与最近播放时间",
			detail: "按播放次数聚合的曲目榜，含首播时间与最近播放时间。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("room top")
				}
				return withCtx(func(ctx context.Context) error { return cmdTop(ctx, args[0]) })
			},
		},
		"policy set": {
			usage: "policy set <room> <JSON>",
			desc:  "设置房间治理策略（管理员）",
			detail: `热更新房间策略，JSON 结构：
  {"max_queue":100,"queue_limits":{"guest":5},"member_player_volume":true}
max_queue 为队列总上限（0=不限）；queue_limits 的 key 匹配身份 kind
（guest/password/oidc）或 role，多命中取最宽松，0/缺省=不限。
member_player_volume 允许同 Room 的 Integration actor 调节 headless output 全局音量，
缺省为 false；普通 WebUI 不受影响。

需要 room_admin 角色。`,
			run: func(args []string) error {
				if len(args) < 2 {
					return errUsage("policy set")
				}
				return withCtx(func(ctx context.Context) error { return cmdPolicySet(ctx, args[0], args[1]) })
			},
		},
		"policy show": {
			usage:  "policy show <room>",
			desc:   "查看房间当前治理策略",
			detail: "显示房间的 policy JSON。",
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("policy show")
				}
				return withCtx(func(ctx context.Context) error { return cmdPolicyShow(ctx, args[0]) })
			},
		},
		"provider qrlogin": {
			usage: "provider qrlogin <provider>",
			desc:  "扫码登录 provider（管理员）",
			detail: `在终端渲染二维码，用对应 App（如网易云音乐）扫码并确认。
凭据由服务端在扫码成功后自动提取、校验并热生效，不经过本机。

需要 media_admin 角色。二维码约 2 分钟过期，全程最多等待 5 分钟。

示例：
  yuzu-cli qrlogin ncm`,
			run: func(args []string) error {
				if len(args) < 1 {
					return errUsage("provider qrlogin")
				}
				return cmdQRLogin(args[0])
			},
		},
	}
}

// groupMeta 子命令组元数据：desc 为组描述，def 为裸组名时的默认子命令（空 = 打印组帮助）。
var groupMeta = map[string]struct {
	desc string
	def  string
}{
	"integration": {"Integration scope 与 subject 管理", "list"},
	"identity":    {"OIDC 身份与 external subject 绑定", ""},
	"principal":   {"Principal 查询", "list"},
	"playlist":    {"通用歌单管理", "list"},
	"queue":       {"队列操作（queue <room> 为查看）", ""},
	"radio":       {"电台模式", ""},
	"policy":      {"房间治理策略", ""},
	"room":        {"房间管理与统计", "list"},
	"provider":    {"Provider 与凭据管理", "list"},
	"media":       {"媒体与缓存", ""},
	"player":      {"播放端管理", "list"},
}

// groupChildren 收集某组的全部子命令名（排序）。
func groupChildren(prefix string) []string {
	var out []string
	for n := range commands {
		if strings.HasPrefix(n, prefix+" ") {
			out = append(out, strings.TrimPrefix(n, prefix+" "))
		}
	}
	sort.Strings(out)
	return out
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
	run := func(c command, rest []string) {
		if err := c.run(rest); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// 最长命令优先：支持 integration scope bind / room controller grant 等三级命令。
	if cmd, rest, ok := commandForArgs(args); ok {
		run(cmd, rest)
	}
	if meta, ok := groupMeta[args[0]]; ok {
		if meta.def != "" {
			run(commands[args[0]+" "+meta.def], args[1:])
		}
		printGroupHelp(args[0])
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s (see 'yuzu-cli help')\n", args[0])
	os.Exit(2)
}

// parseFlagsAnywhere 允许 flag 出现在子命令前后任意位置。
// 带值 flag 消费下一个参数；布尔 flag 不消费（由 flag 包的 IsBoolFlag 判定）。
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
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && !isBoolFlag(name) && i+1 < len(args) {
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

// isBoolFlag 判断已注册 flag 是否为布尔型（布尔 flag 不带值参数）。
func isBoolFlag(name string) bool {
	f := flag.CommandLine.Lookup(name)
	if f == nil {
		return false
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// ---------- 帮助 ----------
func printHelp(args []string) {

	if cmd, _, ok := commandForArgs(args); ok {
		printCommandHelp(cmd)
		return
	}
	if len(args) > 0 {
		if cmd, ok := commands[args[0]]; ok {
			printCommandHelp(cmd)
			return
		}
		if _, ok := groupMeta[args[0]]; ok {
			printGroupHelp(args[0])
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", strings.Join(args, " "))
	}
	fmt.Println(`yuzu-cli — yuzu-jukebox 控制端

用法: yuzu-cli <命令> [参数...] [全局 flag]

命令:`)
	names := make([]string, 0, len(commands))
	for n := range commands {
		if !strings.Contains(n, " ") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		c := commands[n]
		note := ""
		if children := groupChildren(n); len(children) > 0 {
			note = "（子命令: " + strings.Join(children, ", ") + "）"
		}
		fmt.Printf("  %-20s %s%s\n", c.usage, c.desc, note)
	}
	groups := make([]string, 0, len(groupMeta))
	for g := range groupMeta {
		if _, isCmd := commands[g]; !isCmd {
			groups = append(groups, g)
		}
	}
	sort.Strings(groups)
	for _, g := range groups {
		fmt.Printf("  %-20s %s（子命令: %s）\n", g+" <子命令>", groupMeta[g].desc,
			strings.Join(groupChildren(g), ", "))
	}
	fmt.Println(`
全局 flag（可放任意位置，括号内为对应环境变量）:
  -server          服务器地址 (YUZU_SERVER, 默认 http://127.0.0.1:8080)
  -name            显示名 (YUZU_NAME)
  -password        全局管理员口令 (YUZU_PASSWORD)
  -room-password   房间访客密码 (YUZU_ROOM_PASSWORD)

高频收听操作为顶级命令；管理与身份类操作按域分组（room/integration/identity/principal/provider/media/playlist/radio/policy）。
查看详细帮助: yuzu-cli help <命令>，如 yuzu-cli help playlist add`)
}

func printCommandHelp(cmd command) {
	fmt.Printf("用法: yuzu-cli %s\n\n%s\n%s", cmd.usage, cmd.desc, cmd.detail)
	if !strings.HasSuffix(cmd.detail, "\n") {
		fmt.Println()
	}
}

func printGroupHelp(prefix string) {
	meta := groupMeta[prefix]
	fmt.Printf("yuzu-cli %s <子命令> — %s\n\n子命令:\n", prefix, meta.desc)
	for _, sub := range groupChildren(prefix) {
		c := commands[prefix+" "+sub]
		fmt.Printf("  %-26s %s\n", c.usage, c.desc)
	}
	fmt.Printf("\n查看子命令详细帮助: yuzu-cli help %s <子命令>\n", prefix)
}

// commandForArgs 按最长优先匹配最多三级命令，并返回命令参数。
func commandForArgs(args []string) (command, []string, bool) {
	words := len(args)
	if words > 3 {
		words = 3
	}
	for ; words > 0; words-- {
		if cmd, ok := commands[strings.Join(args[:words], " ")]; ok {
			return cmd, args[words:], true
		}
	}
	return command{}, nil, false
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
