# yuzu-jukebox

多房间同步点唱机 / 虚拟音乐大厅。服务器是唯一权威状态源，维护房间播放状态、
管理媒体来源并提供统一出流；客户端各自适配——包括 MPV 播放代理、命令行
控制端、WebUI，以及通过 Integration API 接入的 Chatbot / 游戏 Mod。

## 架构

```
                      ┌──────────── 通用服务端 (Go) ────────────┐
  凭据池 (DB 存储) ──→ │  Provider 层 (local / ncm / ...)         │
                      │    Search / GetTrack / Resolve           │
                      │         ↓ StreamLocator                  │
                      │  Room Actor ×N（状态机 + 队列 + 广播）      │
                      │  流式缓存（边下边播 + LRU）                 │
                      │  WS /ws/v1 · REST /api/v1 · /stream/v1   │
                      └──────┬───────────┬───────────┬──────────┘
                       MPV 代理      控制 CLI      WebUI / 外部集成
                       (纯收听)     (控制/管理)   (浏览器 / IM / Mod)
```

核心设计（详见 [docs/spec-v1.md](docs/spec-v1.md)）：

- **媒体两级模型**：队列里存逻辑引用 `TrackRef`（`ncm:347230`），临近播放才
  `Resolve` 成临时物理地址 `StreamLocator`——签名 URL 过期天然免疫。
- **播放状态是纯函数**：`position_ms + updated_at + playing + rate`，任何客户端
  配合 WS 校时（ping/pong 测 RTT）推算出一致的"此刻应该放到哪"，百毫秒级同步。
- **凭据不出服务器**：MUSIC_U 等凭据存服务端、热更新、先校验再生效；
  客户端凭一次性签发的短时效票据从 `/stream/v1/{track_ref}?ticket=` 拉流。
- **流式缓存**：首次播放边下边播（tee），重放直接命中本地文件，LRU 自动清理。
- **统一外部身份模型**：Integration 把 IM scope 映射到 Room、把外部用户映射到
  持久 Principal；Room controller grant 与 WebUI/CLI 共用同一套服务端授权。

## 快速开始

需要 Go 1.22+。播放代理另需本机安装 [MPV](https://mpv.io/)。

```bash
# 构建
go build -o bin/yuzu-server ./cmd/yuzu-server
go build -o bin/yuzu-agent  ./cmd/yuzu-agent
go build -o bin/yuzu-cli    ./cmd/yuzu-cli

# 启动服务器（首次启动自动生成默认 config.json，按需修改后重启）
./bin/yuzu-server -config config.json
```

`config.json` 示例：

```json
{
  "addr": "127.0.0.1:8080",
  "db_path": "data/yuzu.db",
  "media_dir": "data/media",
  "cache_dir": "data/cache",
  "cache_max_bytes": 21474836480,
  "admin_password": "admin123",
  "integrations": [
    {
      "id": "astrbot-main",
      "token": "replace-with-a-random-secret"
    }
  ],
  "ncm": {
    "enabled": true,
    "base_url": "http://127.0.0.1:3000",
    "level": "exhigh"
  },
  "bili": {
    "enabled": true,
    "base_url": "http://127.0.0.1:3002"
  }
}
```

| 字段 | 说明 |
|---|---|
| `admin_password` | 全局管理员口令；guest 认证时携带即获 `room_admin`/`media_admin` 角色 |
| `cache_max_bytes` | 流式缓存容量上限，超出按 LRU 清理 |
| `ncm` | 网易云 Provider，对接 [NeteaseCloudMusicApi](https://github.com/neteasecloudmusicapienhanced/api-enhanced) 实例；`level` 为音质等级 |
| `bili` | B 站 Provider，对接 bilibili-api sidecar（封装 WBI 签名 / DASH 音频选轨 / 风控）；无 cookie 可匿名 Resolve（320kbps 上限），Search 需扫码登录 |
| `integrations` | 外部 Chatbot / Mod 网关凭据；每个 `id` 对应一个独立 bearer token，token 只配置在受信任的网关进程 |

创建一个房间并上传一首本地音乐：

```bash
# 认证拿 token（带管理员口令）
TOKEN=$(curl -s -X POST localhost:8080/api/v1/auth/guest \
  -H 'Content-Type: application/json' \
  -d '{"name":"admin","password":"admin123"}' | jq -r .session_token)

# 建房（guest_password 是房间访客密码）
curl -X POST localhost:8080/api/v1/rooms -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"lobby","name":"大厅","guest_password":"room123"}'

# 上传媒体（WAV 自动解析时长，其他格式需 ffprobe 或显式传 duration_ms）
curl -X POST localhost:8080/api/v1/media/upload -H "Authorization: Bearer $TOKEN" \
  -F file=@song.wav -F title="My Song"
```

然后一边收听、一边控制：

```bash
# 收听：MPV 播放代理（每个听众一个，纯收听）
./bin/yuzu-agent -room lobby -room-password room123 -name my-mpv

# 控制：CLI
export YUZU_SERVER=http://127.0.0.1:8080
export YUZU_PASSWORD=admin123          # 需要管理员权限的操作
export YUZU_ROOM_PASSWORD=room123

yuzu-cli room list
yuzu-cli search "海阔天空" -provider ncm   # 或 -provider local
yuzu-cli add lobby ncm:347230              # 点歌（空闲时自动开播）
yuzu-cli queue lobby                       # 查看当前播放与队列
yuzu-cli skip lobby                        # 切歌（Room controller）
yuzu-cli pause|resume|seek lobby [秒]       # 播放控制（Room controller）

# 配置 NCM 账号凭据解锁高音质（可选，先校验再生效，热更新）
yuzu-cli provider credential ncm "MUSIC_U=xxxx"

# 或者扫码登录：终端渲染二维码，用网易云 App 扫码确认，凭据自动生效
yuzu-cli provider qrlogin ncm
```

## CLI 参考

全局 flag 可放在子命令前后任意位置，均可用环境变量代替：

| flag | 环境变量 | 说明 |
|---|---|---|
| `-server` | `YUZU_SERVER` | 服务器地址，默认 `http://127.0.0.1:8080` |
| `-name` | `YUZU_NAME` | 显示名 |
| `-password` | `YUZU_PASSWORD` | 全局管理员口令 |
| `-room-password` | `YUZU_ROOM_PASSWORD` | 房间访客密码（mkroom 时作为新房间的密码） |

| 子命令 | 权限 | 说明 |
|---|---|---|
| `room list` | 任意身份 | 列出所有房间 |
| `search <关键词> [-provider p]` | `requester` | 搜索曲目，输出 `track_ref` 供点歌 |
| `queue <room>` | 任意身份 | 查看当前播放（含实时进度）与队列（含电台行） |
| `status <room>` | 任意身份 | 房间总览：播放状态、电台绑定、队列规模、听众 |
| `add <room> <track_ref>...` | `requester` | 点歌，队尾追加，空闲自动开播；多首时原子批量入队 |
| `skip <room>` | Room controller | 切歌，自动播放下一首 |
| `pause / resume <room>` | Room controller | 暂停 / 恢复 |
| `seek <room> <秒>` | Room controller | 跳转进度 |
| `room create <id> <名称>` | `room_admin` | 创建持久房间 |
| `room delete <id>` | `room_admin` | 删除房间（队列与历史级联） |
| `media upload <文件>` | `media_admin` | 上传本地媒体（`-title/-artist/-duration-ms`） |
| `playlist list` | `requester` | 歌单列表 |
| `playlist show <id> [offset]` | `requester` | 歌单条目分页（`-limit`） |
| `playlist create / delete` | `media_admin` | 歌单创建 / 删除 |
| `playlist add <id> <ref>...` | `media_admin` | 追加条目（≤100/次） |
| `playlist delitem <id> <ord>` | `media_admin` | 删除条目 |
| `playlist move <id> <ord> <to_ord>` | `media_admin` | 移动歌单条目 |
| `playlist import <ncm:id|URL|ncm:daily>` | `media_admin` | 导入外部歌单或曲目源快照 |
| `radio play <room> <source>` | Room controller | 电台模式（`-shuffle` / `-once`） |
| `radio stop <room>` | Room controller | 退出电台 |
| `queue del <room> <entry_id>` | 本人 / Room controller | 移除队列条目 |
| `queue move <room> <entry_id> <位置>` | Room controller | 移动队列条目 |
| `room history <room> [offset] [-limit]` | 已认证 | 播放历史（最新在前） |
| `room top <room> [-limit]` | 已认证 | 曲目热度榜（次数、首播/最近） |
| `policy set <room> <JSON>` | `room_admin` | 热更新房间治理策略 |
| `policy show <room>` | 已认证 | 查看房间策略 |
| `player list` | `room_admin` | 在线播放端清单 |
| `player volume <id> <0-100>` | `room_admin` | 远程调音量 |
| `player mute <id> on\|off` | `room_admin` | 远程静音 |
| `player join <id> <room>` | `room_admin` | 迁移播放端到指定房间 |
| `provider credential <provider> <payload>` | `media_admin` | 热更新凭据（先校验再生效） |
| `provider qrlogin <provider>` | `media_admin` | 终端二维码扫码登录，凭据自动生效 |
| `help [命令]` | — | 帮助；`yuzu-cli <命令> --help` 等价 |
| `login` / `logout` / `whoami` | — | OIDC 登录（设备流）/ 登出 / 查看当前身份 |
| `integration list` | `room_admin` | 列出已配置的 Integration（不输出 token） |
| `integration scope list/bind/unbind ...` | `room_admin` | 管理外部频道/群组到默认 Room 的映射 |
| `integration subject list/link/unlink ...` | `room_admin` | 管理外部用户到持久 Principal 的关联 |
| `principal list [query]` | `room_admin` | 按 ID 或名称查询 Principal |
| `room controller list/grant/revoke ...` | `room_admin` | 管理指定 Room 的 controller grant |

### 房间治理策略

每房间可配置点歌限制（`policy set`，热生效）：

```bash
yuzu-cli policy set lobby '{"max_queue": 100, "queue_limits": {"guest": 5, "room_admin": 0}}'
```

`max_queue` 为队列总上限；`queue_limits` 按身份 kind/role 限待播数
（命中 0 = 显式不限）。guest 身份 ID 由名字确定性派生，限额跨会话成立。

### 代理重连

yuzu-agent 内置断线重连：指数退避 1s→30s，重连后自动重走
校时→认证→进房并恢复渲染。服务端重启后，各房间自动续播队首
（当前曲目不持久化，队列保留）。

### OIDC 登录（Zitadel 等 IdP）

服务端可对接 OIDC IdP，组织成员用已有账号登录。全局角色由 IdP role mapping
授予或回收；单个 Room 的 controller 权限由 Yuzu grant 管理：

```jsonc
// config.json
"oidc": {
  "enabled": true,
  "issuer": "https://id.example.org",   // Zitadel 实例域名
  "client_id": "…",                     // Native 应用的 client_id
  "role_mapping": {                     // Zitadel project role → yuzu roles
    "jukebox-admin": ["room_admin", "media_admin"]
  }
}
```

流程：客户端从 IdP 拿到 ID token（CLI/agent 推荐 Device Authorization Grant，
WebUI 推荐 Authorization Code + PKCE）→ `POST /api/v1/auth/oidc` 换 yuzu
session_token → 之后与 guest 完全同构（REST Bearer / WS `auth{session_token}`）。
服务端只验证 ID token（JWKS 本地验签，缓存 + kid 轮换刷新），显示名取
`preferred_username`，身份 ID 由 `sub` 确定性派生（改名不影响权限归属）。

Zitadel console 一次性配置：Native 类型应用（勾 Device Code grant）。角色进
ID token 需 Application 级设置（Token Settings 勾选包含 User Roles 的选项；旧版
UI 叫 "User Roles Inside ID Token"，新版与 Project 级同名为 "Assert Roles on
Authentication"）。Project 级 "Assert Roles on Authentication" 只作用于 userinfo，
可选作兜底。也可由客户端请求 scope `urn:zitadel:iam:org:projects:roles` 替代上述
console 设置。

CLI 登录（推荐路径）：

```bash
yuzu-cli login     # 自动发现服务端 OIDC 配置，终端显示验证链接，浏览器确认
yuzu-cli whoami    # 查看当前身份
yuzu-cli logout    # 清除本地会话缓存
```

登录后会话缓存于 `~/.config/yuzu-cli/session.json`（0600），之后所有命令自动
携带该身份；未登录时回退 guest（`-name`/`-password`）。会话服务端持久化，
服务器重启不掉登录；`logout` 同时在服务端吊销会话。

### 外部 Chatbot / Integration

外部 Bot 使用 `config.json` 中独立的 Integration token 调用 REST API，不应复用
管理员的 session token。服务端按以下链路解析每次请求：

```text
Integration + adapter + scope ──→ 默认 Room
Integration + adapter + scope + subject ──→ Principal
Principal + Room ──→ controller grant
```

未绑定的外部用户会得到稳定的 synthetic guest Principal，只具备普通点歌能力；
绑定到 OIDC Principal 后，外部身份与该成员在 WebUI/CLI 中共享 Principal 和
Room grant。Integration token 只证明调用方是受信任网关，不会自动授予
`room_admin`。

管理员可在 WebUI 的“管理 → 外部集成”完成 scope、subject 和 Room controller
配置，也可使用 CLI：

```bash
yuzu-cli integration list
yuzu-cli integration scope bind astrbot-main astrbot group group-123 lobby
yuzu-cli principal list alice
yuzu-cli integration subject link astrbot-main astrbot group group-123 user-456 o_abc
yuzu-cli room controller grant lobby o_abc
```

解除操作需要传入与创建时相同的完整键；精确参数以
`yuzu-cli help integration`、`yuzu-cli help room` 为准。Bot/Plugin 的运行时
调用流程及 JSON 契约见 [docs/spec-v1.md](docs/spec-v1.md)。

### 电台模式

房间可绑定**曲目源**实现无人值守续播（队列见底自动批量补充）：

```bash
yuzu-cli radio play lobby playlist:pl_xxx -shuffle   # 通用歌单，洗牌袋
yuzu-cli radio play lobby ncm:daily                  # 每日推荐（跨日自动刷新）
yuzu-cli radio play lobby ncm:fm                     # 私人FM（无限流）
yuzu-cli radio play lobby ncm:simi:347230            # 相似歌曲电台（跟随当前曲目）
yuzu-cli radio play lobby ncm:heart:347230           # 心动模式
yuzu-cli radio stop lobby                            # 退出电台
```

无限源（fm/simi/heart）不接受 `-shuffle`/`-once`；链式源有 seen 去重防绕圈。

每个子命令都有独立帮助文档，如 `yuzu-cli search --help`。

## 组件

| 二进制 | 形态 | 职责 |
|---|---|---|
| `yuzu-server` | 常驻服务 | 房间状态机、Provider 管理、流式缓存、WS/REST API |
| `yuzu-agent` | 常驻客户端 | 纯收听：加入房间，把权威播放状态渲染到本地 MPV，自动校偏 |
| `yuzu-cli` | 短命命令 | 搜索、点歌、队列/播放控制、凭据与 Integration 管理 |

官方客户端共享公共 REST/WS 协议；WebUI、Chatbot Plugin、Game Mod 只需按
spec 调用对应能力，服务器不包含平台专用命令解析。

## 项目结构

```
cmd/
  yuzu-server/        # 服务端入口
  yuzu-agent/         # MPV 播放代理（含 MPV JSON IPC 控制）
  yuzu-cli/           # 控制端
internal/
  app/                # 依赖组装（main 与集成测试共用）
  auth/               # Identity/Roles、会话、出流票据
  cache/              # 流式缓存：tee 首拉、singleflight、LRU
  client/             # Go 协议客户端库（agent 与 CLI 共用）
  config/             # JSON 配置
  httpapi/            # REST /api/v1 + 出流 /stream/v1
  provider/           # Provider 抽象（TrackRef/StreamLocator）+ Registry
    local/            # 本地上传媒体
    ncm/              # 网易云音乐（NeteaseCloudMusicApi 实例）
  room/               # 房间 actor：channel 串行状态机，队列持久化
  store/              # SQLite（WAL）+ goose 迁移
  wsapi/              # WS /ws/v1：校时、认证、房间会话
docs/
  spec-v1.md          # 协议与房间状态机权威规格
```

## 部署

生产部署（TLS 反代、systemd、备份、secret_key 注意）见 [docs/deploy.md](docs/deploy.md)。
凭据（ncm/bili cookie）使用 config 的 `secret_key` AES-GCM 加密落盘，历史明文记录自动兼容。

## 测试

```bash
go test ./...          # 含端到端冒烟测试（httptest + WS 全链路）
go test ./... -race
```

## 当前状态与路线

已实现：

- 房间 actor 模型（持久房间、队列持久化、服务端权威时钟、自然结束检测）
- local / ncm / bili 三个 Provider
- 通用歌单（CRUD、分页、ncm 歌单导入、曲目源物化）
- 电台模式（TrackSource 抽象：歌单/每日推荐/私人FM/相似歌曲/心动模式，
  洗牌袋、once、链式种子、seen 去重）
- 房间治理策略（max_queue / 按 kind/role 的点歌限额，热更新）
- 播放历史与热度统计 API（首播时间、播放次数）
- 代理断线重连（指数退避 + 自动恢复渲染）、重启后房间自动续播队首
- 播放端管理平面（player.hello/command/state，远程音量/静音/换房，服务端迁移连接）
- 富元数据（album/cover/source_url/contributors 曲目层 + size/bitrate 物理层、
  封面代理、歌词端点；local 支持 tag/内嵌封面提取）
- 凭据热更新、扫码登录（ncm / bili）、凭据定期健康检查（credmon）
- 流式缓存（tee + 后台续传 + LRU）、下载进度可观测、票据化出流
- MPV 播放代理（防抖渲染）、控制 CLI、WebUI 与端到端冒烟测试
- Guest/password/OIDC Principal、持久会话、Room-scoped controller grant
- Integration actor、外部 scope → Room、subject → Principal 绑定及管理 UI/API
- 凭据 AES-GCM 加密存储与定期健康检查

规划中（spec §8 也列了明确不做的）：

- 更多 Provider
- 投票切歌、DJ 模式等进一步的房间治理策略
