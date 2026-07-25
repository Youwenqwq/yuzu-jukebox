# yuzu-jukebox

多房间同步点唱机 / 虚拟音乐大厅。服务器是唯一权威状态源，维护房间播放状态、
管理媒体来源并提供统一出流；客户端各自适配——可以是 MPV 播放代理、命令行
控制端，或未来的 WebUI、Minecraft Mod。

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
                       MPV 代理      控制 CLI      更多客户端…
                       (纯收听)     (纯控制)      (WebUI / MC Mod)
```

核心设计（详见 [docs/spec-v1.md](docs/spec-v1.md)）：

- **媒体两级模型**：队列里存逻辑引用 `TrackRef`（`ncm:347230`），临近播放才
  `Resolve` 成临时物理地址 `StreamLocator`——签名 URL 过期天然免疫。
- **播放状态是纯函数**：`position_ms + updated_at + playing + rate`，任何客户端
  配合 WS 校时（ping/pong 测 RTT）推算出一致的"此刻应该放到哪"，百毫秒级同步。
- **凭据不出服务器**：MUSIC_U 等凭据存服务端、热更新、先校验再生效；
  客户端凭一次性签发的短时效票据从 `/stream/v1/{track_ref}?ticket=` 拉流。
- **流式缓存**：首次播放边下边播（tee），重放直接命中本地文件，LRU 自动清理。

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
  "ncm": {
    "enabled": true,
    "base_url": "http://127.0.0.1:3000",
    "level": "exhigh"
  }
}
```

| 字段 | 说明 |
|---|---|
| `admin_password` | 全局管理员口令；guest 认证时携带即获 `room_admin`/`media_admin` 角色 |
| `cache_max_bytes` | 流式缓存容量上限，超出按 LRU 清理 |
| `ncm` | 网易云 Provider，对接 [NeteaseCloudMusicApi](https://github.com/neteasecloudmusicapienhanced/api-enhanced) 实例；`level` 为音质等级 |

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

yuzu-cli rooms
yuzu-cli search "海阔天空" -provider ncm   # 或 -provider local
yuzu-cli add lobby ncm:347230              # 点歌（空闲时自动开播）
yuzu-cli queue lobby                       # 查看当前播放与队列
yuzu-cli skip lobby                        # 切歌（管理员）
yuzu-cli pause|resume|seek lobby [秒]       # 播放控制（管理员）

# 配置 NCM 账号凭据解锁高音质（可选，先校验再生效，热更新）
yuzu-cli credential ncm "MUSIC_U=xxxx"

# 或者扫码登录：终端渲染二维码，用网易云 App 扫码确认，凭据自动生效
yuzu-cli qrlogin ncm
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
| `rooms` | 任意身份 | 列出所有房间 |
| `search <关键词> [-provider p]` | `requester` | 搜索曲目，输出 `track_ref` 供点歌 |
| `queue <room>` | 任意身份 | 查看当前播放（含实时进度）与队列 |
| `add <room> <track_ref>` | `requester` | 点歌，队尾追加，空闲自动开播 |
| `skip <room>` | `room_admin` | 切歌，自动播放下一首 |
| `pause / resume <room>` | `room_admin` | 暂停 / 恢复 |
| `seek <room> <秒>` | `room_admin` | 跳转进度 |
| `mkroom <id> <名称>` | `room_admin` | 创建持久房间 |
| `upload <文件>` | `media_admin` | 上传本地媒体（`-title/-artist/-duration-ms`） |
| `credential <provider> <payload>` | `media_admin` | 热更新凭据（先校验再生效） |
| `qrlogin <provider>` | `media_admin` | 终端二维码扫码登录，凭据自动生效 |
| `help [命令]` | — | 帮助；`yuzu-cli <命令> --help` 等价 |

每个子命令都有独立帮助文档，如 `yuzu-cli search --help`。

## 组件

| 二进制 | 形态 | 职责 |
|---|---|---|
| `yuzu-server` | 常驻服务 | 房间状态机、Provider 管理、流式缓存、WS/REST API |
| `yuzu-agent` | 常驻客户端 | 纯收听：加入房间，把权威播放状态渲染到本地 MPV，自动校偏 |
| `yuzu-cli` | 短命命令 | 纯控制：搜索、点歌、队列管理、播放控制、凭据管理 |

两类客户端共享 `internal/client` 协议库；写新客户端（WebUI、MC Mod）只需
按 spec 实现协议，服务器无需改动。

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

## 测试

```bash
go test ./...          # 含端到端冒烟测试（httptest + WS 全链路）
go test ./... -race
```

## 当前状态与路线

已实现：

- 房间 actor 模型（持久房间、队列持久化、服务端权威时钟、自然结束检测）
- local Provider（上传、WAV/ffprobe 时长探测）与 ncm Provider（搜索/详情/Resolve）
- 凭据热更新（先校验再生效）、流式缓存、票据化出流
- MPV 播放代理、控制 CLI、端到端冒烟测试

规划中（spec §8 也列了明确不做的）：

- 更多 Provider（B 站等）
- 账号密码 / OIDC 登录（Roles 模型已预留）
- WebUI 客户端
- 投票切歌、DJ 模式等房间治理策略（`policy_json` 字段已预留）
- 凭据加密存储
