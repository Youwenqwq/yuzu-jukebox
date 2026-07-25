# Yuzu Jukebox 协议与房间状态机规格 v1

本文档是客户端与服务端对接的唯一权威规格。实现与本文冲突时，以本文为准。

## 0. 总则

- 服务端是唯一权威状态源；客户端是状态的"渲染器"。
- 所有时间戳为 **Unix 毫秒（UTC）**。
- 通信分两条通道，职责严格分离：
  - **WebSocket `/ws/v1`**：实时会话——校时、房间状态广播、队列/播放操作。
  - **REST `/api/v1/*`**：低频管理——认证、搜索、房间管理、媒体上传、凭据管理。
  - **出流 `/stream/v1/{track_ref}`**：统一媒体流端点。
- 所有端点路径携带主版本号。版本纪律：
  - **只有破坏性变更才升版本**（删字段、改字段类型/语义、改鉴权方式、改状态机行为）。
  - 新增字段、消息类型、端点不算破坏性，直接在现版本内添加。
  - 客户端实现 MUST 忽略 JSON 中的未知字段。
  - 新旧版本端点可并行运行，便于老客户端（游戏 Mod、播放代理）平滑迁移。

## 1. 消息信封

WS 上所有消息为 JSON 文本帧：

```json
{ "type": "<消息类型>", "ref": "<可选，客户端生成的请求ID>", "data": { ... } }
```

- 客户端发起的请求类消息 SHOULD 携带 `ref`（如递增序号或 UUID）。
- 服务端对该请求的 `ack` 或 `error` 响应 MUST 原样回带 `ref`。
- 广播类消息（状态变更）无 `ref`。

## 2. 时钟同步

### 2.1 流程

连接建立后、认证前，客户端进行 3–5 轮校时：

```json
→ { "type": "ping", "data": { "client_time": 1720000000000 } }
← { "type": "pong", "data": { "client_time": 1720000000000, "server_time": 1720000000123 } }
```

客户端算法：

```
rtt    = recv_time - client_time
offset = server_time + rtt/2 - recv_time
取 rtt 最小的样本的 offset
server_now() = local_now() + offset
```

### 2.2 约束

- 客户端 MUST 在加入房间前完成校时；播放期间 MAY 每 60s 重新校时一次修正漂移。
- 播放位置推算：

```
should_be = position_ms + (server_now() - updated_at) * rate   (playing 时)
should_be = position_ms                                        (paused 时)
```

- 对齐策略（客户端尽力而为，协议不强制）：
  - |偏差| > 150ms → seek
  - 30–150ms → 调 playbackRate（0.98–1.02）缓追
  - < 30ms → 不动

## 3. 认证

### 3.1 访客登录（首个版本唯一实现）

```json
→ { "type": "auth", "ref": "1", "data": { "name": "阿柚", "password": "房间访客密码" } }
← { "type": "auth.ok", "ref": "1", "data": {
      "identity": { "id": "g_8f3k2", "name": "阿柚", "kind": "guest",
                    "roles": ["listener", "requester"] },
      "session_token": "..."
  } }
```

- 访客密码为**每房间一个**，在 join 时校验（见 4.2）；`auth` 阶段仅登记身份。
- 后续版本新增 `password` / `oidc` 登录，`Identity` 结构不变。
- 所有操作的服务端鉴权 MUST 只检查 `roles`，不检查 `kind`。

## 4. 房间会话

### 4.1 房间快照（`room.state`）

- `room.join` 成功时服务端依次推送 `room.joined`（回带 ref）→ `playback.changed` → `queue.changed` → `listeners.changed`（全量快照三件套）。此后客户端靠增量事件维护本地副本：

```json
{
  "room_id": "lobby",
  "playback": {
    "current": {
      "entry_id": "e_991", "track_ref": "netease:123456",
      "title": "...", "artist": "...", "duration_ms": 245000,
      "requested_by": "g_8f3k2",
      "stream_url": "/stream/v1/netease:123456?ticket=tk_abc"
    },
    "position_ms": 30210,
    "updated_at": 1720000000123,
    "playing": true,
    "rate": 1.0
  },
  "queue": [ { "entry_id": "...", "track_ref": "...", "...": "..." } ],
  "listeners": [ { "id": "...", "name": "..." } ]
}
```

### 4.2 客户端 → 服务端消息

| type | data | 权限 | 说明 |
|---|---|---|---|
| `room.join` | `{room_id, password}` | 任何已认证身份 | 密码错误返回 `error` |
| `room.leave` | `{room_id}` | — | |
| `queue.add` | `{room_id, track_ref}` | `requester` | 队尾追加 |
| `queue.remove` | `{room_id, entry_id}` | `requester`(仅自己的) / `room_admin` | |
| `queue.move` | `{room_id, entry_id, to_index}` | `room_admin` | |
| `playback.pause` / `playback.resume` | `{room_id}` | `room_admin` | |
| `playback.seek` | `{room_id, position_ms}` | `room_admin` | |
| `playback.skip` | `{room_id}` | `room_admin` | 切下一首 |

### 4.3 服务端 → 客户端事件

| type | 时机 | data 要点 |
|---|---|---|
| `playback.changed` | 播放/暂停/seek/切歌/自然结束 | 完整 `playback` 对象（同快照） |
| `queue.changed` | 队列任何变更 | 完整 `queue` 数组（v1 全量下发，量大后再做 diff） |
| `listeners.changed` | 听众进出 | 完整 `listeners` 数组 |
| `error` | 请求失败 | `{code, message}` + 回带 `ref` |

**关键设计：`playback.changed` 永远携带完整 playback 对象。** 客户端无需区分事件原因，统一按"以最新状态重算 should_be"处理。新增听众也凭此一次性对齐。

### 4.4 出流票据

- `stream_url` 中的 `ticket` 为短时效（5 分钟）、绑定身份与 track 的令牌。
- 票据在 TTL 内**可复用**：客户端的 Range 请求与断线重试都依赖同一 URL。曲目播完（切歌/自然结束）时该曲目所有票据立即失效。
- 目的：凭据永不下发；`/stream` 端点无 cookie 会话也可鉴权（`<audio>` 标签、MPV 均适用）。
- 仅**当前播放曲目**附带 `stream_url`；队列条目只给元数据。下一首的可播放性由服务端缓存预取保证（进入 PLAYING 时对队首做 Resolve + 预拉取），待其成为当前曲目时才下发 URL。

## 5. 房间状态机

### 5.1 状态字段（权威，存于房间 actor 内，不落库）

```go
PlaybackState {
    Current     *QueueEntry  // nil = 空闲
    PositionMs  int64        // 语义锚点见下
    UpdatedAt   int64        // PositionMs 对应的服务器时钟时刻
    Playing     bool
    Rate        float64      // v1 恒为 1.0
}
Queue []QueueEntry
```

不变量：**任何时刻任意观察者由这五元组算出的播放位置一致**；系统内不存在"当前进度"这种需要持续刷新的字段。

### 5.2 状态迁移

```
                 queue.add (空闲时)
        ┌─────────┴──────────┐
        ▼                    │
     [空闲 IDLE]          有 current
        │                    │
        │ 取队首, Resolve,   │
        │ 缓存命中检查        │
        ▼                    │
   [播放中 PLAYING] ◄────────┘
     │  ▲   │
pause│  │resume
     ▼  │   │seek (回到 PLAYING，仅更新 position/updated_at)
   [已暂停 PAUSED]
     
  PLAYING ──自然结束(timer: position + duration/rate)──┐
  PLAYING ──playback.skip─────────────────────────────┤
                                                       ▼
                                          队列非空 → 取下一首 → PLAYING
                                          队列为空 → IDLE
```

迁移规则：

1. **pause**：`PositionMs = position(now)`；`Playing = false`；`UpdatedAt = now`。
2. **resume**：`PositionMs` 不变；`Playing = true`；`UpdatedAt = now`。
3. **seek(p)**：`PositionMs = p`；`UpdatedAt = now`；`Playing` 不变。越界 clamp 到 `[0, duration]`。
4. **skip / 自然结束**：当前条目写入 `play_history`（`end_reason = skipped|finished`），取队首。队首条目的 `stream_url` 在此刻生成票据并随广播下发。
5. **自然结束检测**：房间 actor 在每次进入 PLAYING 时按 `剩余时长/rate` 设一次性 timer；期间任何 pause/seek/skip 重置该 timer。不依赖客户端上报。
6. **预解析**：进入 PLAYING 时若队首为下一首，立即对其 Resolve 并预拉取缓存前 N MB（N 建议 8），使切歌时 `/stream` 首字节无源站延迟。

### 5.3 并发模型

- 每个房间一个 actor goroutine；所有 4.2 节操作、timer 事件、听众进出全部经 `inbound` channel 串行处理。无锁。
- 广播由 actor 组装一次快照，扇出给各 listener 的发送队列；慢客户端（发送缓冲满）直接断开，由客户端重连 + 重新 join 拿全量快照恢复。

## 6. REST API 概览（/api/v1）

| 端点 | 权限 | 说明 |
|---|---|---|
| `POST /api/v1/auth/guest` | — | WS 外的替代认证入口（供不便在 WS 上认证的客户端） |
| `GET /api/v1/rooms` | 已认证 | 房间列表（大厅目录） |
| `POST /api/v1/rooms` / `PATCH /api/v1/rooms/{id}` | `room_admin` | 后台建/改房间（名称、访客密码、policy_json） |
| `GET /api/v1/search?provider=&q=` | `requester` | 转发 Provider.Search，返回 Track 列表（含 track_ref） |
| `GET /api/v1/providers` | `requester` | 已注册的 Provider 列表 |
| `POST /api/v1/providers/{id}/credential` | `media_admin` | 热更新 Provider 凭据（如 ncm 的 MUSIC_U cookie）；服务端先校验再生效，凭据存 credentials 表，永不下发 |
| `POST /api/v1/media/upload` | `media_admin` | local provider 上传 |
| `GET /api/v1/media/cache` / `DELETE /api/v1/media/cache/{track_ref}` | `media_admin` | 缓存查看/手动清理 |
| `GET /stream/v1/{track_ref}?ticket=` | 持票 | 统一出流；支持 HTTP Range |

## 7. 错误码

| code | 含义 |
|---|---|
| `unauthorized` | 未认证 / 会话失效 |
| `forbidden` | 角色权限不足 |
| `bad_request` | 参数非法（含 track_ref 格式错误） |
| `not_found` | 房间 / 条目 / 曲目不存在 |
| `provider_error` | Provider 调用失败（附 message） |
| `rate_limited` | 预留 |

## 8. 明确不做的（当前版本）

- 投票切歌、DJ 模式（policy_json 预留字段，不实现逻辑）
- 播放进度持久化（重启后房间恢复为 IDLE，队列保留在 DB）
- 队列事件的增量 diff 下发
- OIDC / 账号密码登录
- 凭据加密存储（v1 明文存 credentials 表；加密需引入密钥管理，随凭据种类增多再做）
