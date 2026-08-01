# Yuzu Jukebox

自托管的多房间音乐点唱机——从多个来源点歌、跨房间同步播放，
通过 CLI、WebUI 或聊天机器人控制一切。

> **开发阶段说明**：当前版本仍处于开发阶段，基本功能已经可用，可搭配 [yuzu-jukebox-webui](https://github.com/Youwenqwq/yuzu-jukebox-webui) 使用；接口定义可能随时变更，不建议用于生产环境。

## 能做什么？

- **多房间播放** — 每个房间独立队列、独立播放状态、独立听众
- **多来源支持** — 本地文件、网易云音乐，更多来源开发中...
- **实时同步** — WebSocket 推送播放状态，所有客户端百毫秒级对齐
- **无头播放** — `yuzu-agent` 装在任何有 MPV 的机器上，充当房间音箱
- **聊天机器人集成** — 通过 Integration API 接入 Discord/Telegram/IM Bot
- **电台模式** — 歌单随机、每日推荐、私人 FM、相似歌曲电台
- **OIDC 登录** — 对接 Zitadel 等 IdP，角色映射到 Yuzu 权限
- **流式缓存** — 首次播放边下边存，重放命中本地文件，LRU 自动清理

## 工作原理

```
You (CLI / WebUI / Chat bot)
        │
        ▼
┌──────────────────────────────┐
│        yuzu-server           │
│  Room state machines         │
│  Queue + playback control    │
│  Provider layer (NCM/Bili)   │
│  Stream cache + ticket auth  │
└──────────┬───────────────────┘
           │
    ┌──────┴──────┐
    ▼             ▼
yuzu-agent     Web browser
(MPV speaker)  (HTML5 Audio)
```

- **Server** 是唯一权威状态源——房间、队列、播放状态
- **Agent** 是无头 MPV 渲染器，作为持久音箱绑定到房间
- **Client**（CLI、WebUI、Bot）通过 REST + WebSocket 控制播放

## 技术栈

| 层 | 技术 |
|---|------|
| 服务端 | Go 1.26+，`net/http` 标准库，WebSocket |
| 数据库 | SQLite（WAL 模式，goose 迁移） |
| 播放代理 | MPV（JSON IPC） |
| 认证 | Guest、OIDC（Zitadel）、会话令牌、流票据鉴权 |
| Provider | NeteaseCloudMusicApi（sidecar 进程）；Bilibili 暂不可用 |

## 快速开始

**前置条件：** Go 1.26+。播放代理需要 [MPV](https://mpv.io/)。

```bash
# 构建
go build -o bin/yuzu-server ./cmd/yuzu-server
go build -o bin/yuzu-agent  ./cmd/yuzu-agent
go build -o bin/yuzu-cli    ./cmd/yuzu-cli

# 启动（首次运行自动生成 config.json）
./bin/yuzu-server -config config.json
```

编辑 `config.json` 启用 Provider：

```json
{
  "addr": ":8080",
  "admin_password": "change-me",
  "ncm": { "enabled": true, "base_url": "http://127.0.0.1:3000", "level": "exhigh" }
}
```

NCM 需要运行 sidecar API——见下方[外部依赖](#外部依赖)。

## 基本使用

认证、创建房间、开始点歌：

```bash
export YUZU_SERVER=http://127.0.0.1:8080
export YUZU_PASSWORD=change-me

# 管理员认证
TOKEN=$(curl -s -X POST $YUZU_SERVER/api/v1/auth/guest \
  -H 'Content-Type: application/json' \
  -d '{"name":"admin","password":"change-me"}' | jq -r .session_token)

# 创建房间
curl -X POST $YUZU_SERVER/api/v1/rooms -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"lobby","name":"大厅"}'

# 搜索并点歌
yuzu-cli search "海阔天空" -provider ncm
yuzu-cli add lobby ncm:347230

# 播放控制
yuzu-cli pause lobby
yuzu-cli resume lobby
yuzu-cli skip lobby
yuzu-cli queue lobby
```

### 常用 CLI 命令

| 命令 | 说明 |
|------|------|
| `yuzu-cli search <关键词> [-provider local\|ncm]` | 搜索曲目 |
| `yuzu-cli add <房间> <track_ref>...` | 点歌入队 |
| `yuzu-cli queue <房间>` | 查看当前播放与队列 |
| `yuzu-cli skip <房间>` | 切歌 |
| `yuzu-cli pause\|resume\|seek <房间> <秒>` | 播放控制 |
| `yuzu-cli room list` | 列出房间 |
| `yuzu-cli room create <id> <名称>` | 创建房间 |
| `yuzu-cli radio play <房间> playlist:<id> -shuffle` | 开启电台 |
| `yuzu-cli radio stop <房间>` | 停止电台 |
| `yuzu-cli media upload <文件>` | 上传本地音频 |
| `yuzu-cli provider qrlogin ncm` | 扫码登录网易云 |
| `yuzu-cli player create <id> <名称>` | 创建音箱 |
| `yuzu-cli player bind <id> <房间>` | 分配音箱到房间 |
| `yuzu-cli login` | OIDC 设备登录 |

每个命令都有 `--help`。运行 `yuzu-cli <命令> --help` 查看详情。

## 配置音箱

创建持久 Player，绑定到房间，然后启动 Agent：

```bash
yuzu-cli player create living-room "客厅音箱"
# 保存输出中的一次性 key！
yuzu-cli player bind living-room lobby

# 在音箱机器上（默认连 http://127.0.0.1:8080；音箱在其他机器时用 YUZU_SERVER 指定服务器）：
YUZU_SERVER=http://<服务器地址>:8080 YUZU_PLAYER_KEY=yzp_xxx ./bin/yuzu-agent
```

Agent 自动断线重连、同步播放进度，始终绑定到已分配的房间。

## 外部依赖

以下作为独立 sidecar 进程运行：

| Provider | Sidecar | 默认地址 |
|----------|---------|---------|
| 网易云音乐 | [NeteaseCloudMusicApi](https://github.com/neteasecloudmusicapienhanced/api-enhanced) | `http://127.0.0.1:3000` |
| Bilibili | 暂不可用 | — |

在 config 中设 `"enabled": false` 即可禁用对应 Provider。

## 部署

生产环境部署详见 [docs/deploy.md](docs/deploy.md)：
- 反向代理（Caddy 或 nginx）+ TLS 终结
- systemd 服务单元
- 备份策略
- 公域与私域部署考量

**运行环境：** Linux 服务器（x86/ARM）。Agent 可运行在任何有 MPV 的 Linux 机器上——树莓派、旧笔记本皆可。

公网源站带宽较小时，可评估 [EdgeOne CDN 旁路分发](docs/edgeone-distribution.md)——**实验性功能，仍在开发中，暂不建议使用**。

## 文档

| 文档 | 内容 |
|------|------|
| [docs/spec-v1.md](docs/spec-v1.md) | 协议、房间状态机、认证流程、Integration 契约 |
| [docs/deploy.md](docs/deploy.md) | 生产环境部署指南 |
| [docs/edgeone-distribution.md](docs/edgeone-distribution.md) | EdgeOne CDN 旁路设计（实验性） |
| [AGENTS.md](AGENTS.md) | 代码库指南（面向贡献者与 Coding Agent） |

## 权限

| 角色 | 能力 |
|------|------|
| `listener` | 加入房间、查看状态、接收播放 |
| `requester` | 搜索、点歌、删除自己的待播条目 |
| `room_admin` | 管理房间、Integration 与授权；所有房间的 controller |
| `media_admin` | 上传媒体、管理 Provider 凭据与歌单 |
| Room `controller` 授权 | 控制指定房间的播放 |

## License

[AGPL-3.0](LICENSE)
