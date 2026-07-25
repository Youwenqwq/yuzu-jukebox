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
  - 30–150ms → 微调 playbackRate（0.98–1.02）
  - < 30ms → 不动
  - **输出延迟区分**：播放器位置读数可能系统性少报（蓝牙等输出链路延迟被播放器从 time-pos 中扣除，典型 200–300ms）。客户端应把“连续多次采样漂移值稳定”判定为测量偏差并停止纠正——否则会造成每秒一次的无效 seek 风暴（参考实现 yuzu-agent 的 driftSyncer：连续 3 次 ±60ms 判定）。

## 3. 认证

### 3.1 访客登录

```json
→ { "type": "auth", "ref": "1", "data": { "name": "阿柚", "password": "" } }
← { "type": "auth.ok", "ref": "1", "data": {
      "identity": { "id": "g_8f3k2", "name": "阿柚", "kind": "guest",
                    "roles": ["listener", "requester"] },
      "session_token": "..."
  } }
```

- `password` 字段是**全局管理员口令**（可选）：命中服务端配置的 `admin_password` 时，
  roles 追加 `room_admin` 与 `media_admin`；不传或错误则为普通访客（`listener` + `requester`）。
- **房间访客密码不在此处传递**——它是每房间一个的进门凭证，在 `room.join` 时校验（见 4.2）。
- `session_token` 用于 REST 通道鉴权（见 §6）；WS 通道上 `auth.ok` 之后的操作直接用该连接的身份。
- guest 身份 ID 由名字确定性派生（`g_` + sha256(name) 前 12 hex），同名重连仍是同一人。
- 会话存于服务端内存：**连接断开后必须重新 `auth`**。
- 所有操作的服务端鉴权 MUST 只检查 `roles`，不检查 `kind`。

### 3.2 会话 token 认证（OIDC 等 REST 登录后的 WS 接入）

```json
→ { "type": "auth", "ref": "1", "data": { "session_token": "..." } }
← { "type": "auth.ok", ... }   // 与 3.1 同构
```

凡经 REST 获得的 session_token（guest REST 登录、OIDC 登录）都可直接在 WS 上换取连接身份。

### 3.3 OIDC 登录（Zitadel 等 IdP）

```
POST /api/v1/auth/oidc { "id_token": "<IdP 签发的 ID token>" }
→ { "identity": { "id": "o_…", "name": "<preferred_username>", "kind": "oidc",
                  "roles": ["listener", "requester", …映射结果] },
    "session_token": "..." }
```

- 服务端只验证 **ID token**（恒为 JWT，RS256）；access token（Zitadel 默认 opaque）不使用。
- 验签材料：`{issuer}/.well-known/openid-configuration` → `jwks_uri` → JWKS 缓存；未知 kid 自动刷新一次（密钥轮换）。校验 iss / aud（= config `oidc.client_id`）/ exp（60s 宽限）。
- 显示名取 `preferred_username`（ID token 恒有；`name` 在颁发 access token 时会被 Zitadel 从 ID token 剥掉，不可用）。
- 身份 ID：`o_` + sha256(sub) 前 12 hex——`sub` 在 IdP 里永不变，改名不影响归属。
- 角色映射：扫描 payload 中所有 `urn:zitadel:iam:org:project*:roles` claim，对象 key 即角色名；config `oidc.role_mapping` 把 Zitadel 角色名映射为 yuzu roles。未命中者保持 `listener + requester`。
- IdP 侧前提（Zitadel console）：Native 类型应用；Project 勾 "Assert Roles on Authentication"；Application Token Settings 勾 "User Roles Inside ID Token"。
- 客户端获取 id_token 的方式服务端不感知：CLI/agent 推荐 Device Authorization Grant；WebUI 推荐 Authorization Code + PKCE。

## 4. 房间会话

### 4.1 房间快照

`room.join` 成功时，服务端按固定顺序推送五条消息：

1. `room.joined`（回带 ref，data: `{"room_id": "..."}`）
2. `playback.changed` —— 当前播放状态
3. `queue.changed` —— 完整队列
4. `radio.changed` —— 电台模式状态（见 4.5）
5. `listeners.changed` —— 完整听众列表

三条快照消息此后有任何变更都会再次全量推送，客户端始终用最新一份覆盖本地副本。

**`playback.changed` 的 data（即 playback 对象本身，无外层包装）：**

```json
{
  "current": {
    "entry_id": "e_991", "track_ref": "ncm:347230",
    "title": "海阔天空", "artist": "Beyond", "duration_ms": 326000,
    "requested_by": "g_8f3k2", "added_at": 1720000000000,
    "stream_url": "/stream/v1/ncm:347230?ticket=tk_abc"
  },
  "position_ms": 30210,
  "updated_at": 1720000000123,
  "playing": true,
  "rate": 1.0
}
```

- 空闲时 `current` 为 `null`、`playing` 为 `false`。
- `stream_url` 仅出现在 `current` 上，且**按接收身份签发**（见 4.4），不同听众收到的票据不同。

**`queue.changed` 的 data：**`{"queue": [ <队列条目>, ... ]}`——条目字段同 `current`（但无 `stream_url`）。

**`listeners.changed` 的 data：**`{"listeners": [ {"id": "...", "name": "..."}, ... ]}`

**`radio.changed` 的 data：**`{"radio": null | {"source": "...", "description": "...", "finite": true, "shuffle": false, "once": false}}`（见 4.5）

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
| `radio.play` | `{room_id, source, shuffle, once}` | `room_admin` | 进入电台模式（见 4.5） |
| `radio.stop` | `{room_id}` | `room_admin` | 退出电台模式 |

### 4.3 服务端 → 客户端消息

| type | 时机 | data 要点 |
|---|---|---|
| `ack` | 请求处理成功 | 空对象；**回带 `ref`** |
| `error` | 请求失败 | `{code, message}`；**回带 `ref`** |
| `room.joined` | `room.join` 成功 | `{"room_id"}`；回带 `ref` |
| `room.left` | `room.leave` 成功 | 空对象；回带 `ref` |
| `pong` | 响应 `ping` | `{client_time, server_time}`；回带 `ref` |
| `playback.changed` | 播放/暂停/seek/切歌/自然结束/新听众进房 | 完整 `playback` 对象（见 4.1） |
| `queue.changed` | 队列任何变更 | `{"queue": [...]}`（当前版本全量下发，量大后再做 diff） |
| `listeners.changed` | 听众进出 | `{"listeners": [...]}` |
| `radio.changed` | 电台开启/停止/新听众进房 | `{"radio": null | {...}}` |

**请求-响应匹配**：除广播三件套（`playback/queue/listeners.changed`）外，所有服务端消息都回带触发它的请求的 `ref`。客户端 MUST 用 `ref` 把响应路由到对应请求；无匹配 `ref` 的消息按广播处理。

**关键设计：`playback.changed` 永远携带完整 playback 对象。** 客户端无需区分事件原因，统一按"以最新状态重算 should_be"处理。新增听众也凭此一次性对齐。

### 4.5 电台模式与曲目源

房间可绑定一个**曲目源（TrackSource）**进入电台模式：队列见底（<3 首）时服务端自动从源批量补充（10 首），实现无人值守续播。用户点歌与补充批次自然混排。

**源规格字符串**（`radio.play` 的 `source` 字段）：

| 规格 | 类型 | 说明 |
|---|---|---|
| `playlist:<id>` | 有限 | 通用歌单；顺序循环 / once；`-shuffle` 为洗牌袋 |
| `ncm:daily` | 有限 | 每日推荐；TTL 物化，跨日自动重拉 |
| `ncm:simi:<song_id>` | 无限 | 相似歌曲链式游走：种子初始为规格 id，之后跟随当前播放曲目 |
| `ncm:heart:<song_id>` | 无限 | 心动模式：种子 = 登录账号"我喜欢" + 当前播放曲目 |
| `ncm:fm` | 无限 | 私人FM：每批新 API 调用，不耗尽 |

规则：

- 无限源（`finite=false`）**拒绝** `shuffle`/`once`（返回 `bad_request`）。
- 链式/无限源内部维护 seen 集合（约 500 条滚动窗口）防重复；整批均重复时判定耗尽，服务端自动停止电台并广播 `radio.changed{null}`。
- 源出错（如凭据失效）时服务端停止电台并广播 `radio.changed{null}`，服务端日志留痕。
- 电台状态为**运行时状态**：不落库，重启即止（队列保留）。
- `radio.play` 切换源时**不清空**现有队列；新源在队列见底后才开始补充。

### 4.6 出流票据

- `stream_url` 中的 `ticket` 为短时效（5 分钟）、绑定身份与 track 的令牌。
- 票据在 TTL 内**可复用**：客户端的 Range 请求与断线重试都依赖同一 URL。曲目播完（切歌/自然结束）时该曲目所有票据立即失效。
- 目的：凭据永不下发；`/stream` 端点无 cookie 会话也可鉴权（`<audio>` 标签、MPV 均适用）。
- 仅**当前播放曲目**附带 `stream_url`；队列条目只给元数据。下一首的可播放性由服务端缓存预取保证（进入 PLAYING 时对队首做 Resolve + 预拉取），待其成为当前曲目时才下发 URL。

### 4.7 房间治理策略

每房间一份策略，存 `rooms.policy_json`，经 `POST/PATCH /api/v1/rooms` 的 `policy` 字段热更新（校验 → 落库 → actor 内生效，无需重启）：

```json
{ "max_queue": 100, "queue_limits": { "guest": 5, "room_admin": 0 } }
```

- `max_queue`：待播队列总上限，0/缺省 = 不限。超限拒绝 `queue.add`，错误码 `queue_full`。
- `queue_limits`：按身份的待播数上限。key 匹配身份的 **kind**（`guest`/`password`/`oidc`）或其任一 **role**；任一命中值为 0 → 显式不限（覆盖其他命中），否则取命中最大值，无命中 = 不限。超限拒绝，错误码 `quota_exceeded`。
- guest 身份 ID 由名字确定性派生（`g_` + sha256(name) 前 12 hex），同名重连仍是同一人——限额与"移除自己点的歌"跨会话成立。
- 电台补充不经过限额检查（补充只在队列 <3 时触发，天然有界）。
- 播放历史与统计见 6 节 REST；`play_history` 只记**播放**（结束原因 `finished`/`skipped`），不记点歌意图。

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
6. **毒化条目丢弃**：`duration_ms <= 0` 的队列条目在 advance 时直接丢弃（记日志，不写历史）——时长未知会导致结束 timer 永不触发，队列永久停滞。单次 advance 最多连续丢弃 100 条，防病态电台源死循环。
7. **预解析**：进入 PLAYING 时若队列非空，立即对新队首 Resolve 并后台预拉取缓存（整文件），使切歌时 `/stream` 首字节无源站延迟。

### 5.3 并发模型

- 每个房间一个 actor goroutine；所有 4.2 节操作、timer 事件、听众进出全部经 `inbound` channel 串行处理。无锁。
- 广播由 actor 组装一次快照，扇出给各 listener 的发送队列；慢客户端（发送缓冲满）直接断开，由客户端重连 + 重新 join 拿全量快照恢复。

## 6. REST API 概览（/api/v1）

REST 请求统一经 `Authorization: Bearer <session_token>` 鉴权（token 来自 `POST /api/v1/auth/guest` 或 WS `auth.ok`）。

| 端点 | 权限 | 说明 |
|---|---|---|
| `POST /api/v1/auth/guest` | — | 访客认证，body `{"name", "password"}`（password 为全局管理员口令，可选）；返回 `{identity, session_token}` |
| `POST /api/v1/auth/oidc` | — | OIDC 认证，body `{"id_token"}`（IdP 签发的 ID token）；服务端验签 + 角色映射后返回 `{identity, session_token}`；未启用 404 |
| `GET /api/v1/rooms` | 已认证 | 房间列表（大厅目录） |
| `POST /api/v1/rooms` / `PATCH /api/v1/rooms/{id}` | `room_admin` | 后台建/改房间（名称、访客密码、policy） |
| `GET /api/v1/rooms/{id}/history?offset=&limit=` | 已认证 | 播放历史，最新在前（默认 50，上限 200） |
| `GET /api/v1/rooms/{id}/stats?limit=` | 已认证 | 曲目热度榜：播放次数、首播/最近播放时间（默认 20，上限 100） |
| `GET /api/v1/search?provider=&q=` | `requester` | 转发 Provider.Search，返回 Track 列表（含 track_ref） |
| `GET /api/v1/providers` | `requester` | 已注册的 Provider 列表 |
| `POST /api/v1/providers/{id}/credential` | `media_admin` | 热更新 Provider 凭据（如 ncm 的 MUSIC_U cookie）；服务端先校验再生效，凭据存 credentials 表，永不下发 |
| `POST /api/v1/providers/{id}/qrlogin` | `media_admin` | 生成二维码登录会话，返回 `{key, qr_content}`（客户端自行渲染二维码） |
| `GET /api/v1/providers/{id}/qrlogin/{key}` | `media_admin` | 轮询扫码状态：waiting / scanned / ok / expired；ok 时服务端已自行提取 MUSIC_U、校验并热生效（cookie 不经过客户端） |
| `GET /api/v1/playlists` | `requester` | 歌单列表（含曲目数，不含条目） |
| `POST /api/v1/playlists` | `media_admin` | 创建歌单 `{name, description}` |
| `GET /api/v1/playlists/{id}?offset=&limit=` | `requester` | 歌单详情 + 条目分页（默认 50，上限 200） |
| `DELETE /api/v1/playlists/{id}` | `media_admin` | 删除歌单 |
| `POST /api/v1/playlists/{id}/items` | `media_admin` | 追加条目 `{track_refs: [...]}`（单次≤100，元数据实时快照） |
| `DELETE /api/v1/playlists/{id}/items/{ord}` | `media_admin` | 按序号删条目，后续重排 |
| `POST /api/v1/playlists/import` | `media_admin` | 导入：`{provider, playlist_id}`（外部歌单，ncm 支持 id 或 URL，分页拉全量）或 `{source}`（曲目源物化，如 `ncm:daily`）；可选 `{name}` |
| `POST /api/v1/media/upload` | `media_admin` | local provider 上传 |
| `GET /api/v1/media/cache` | `media_admin` | 缓存全貌：`entries`（已缓存）、`downloads`（进行中，含进度）、`history`（最近 20 条成功/失败记录，内存态） |
| `DELETE /api/v1/media/cache/{track_ref}` | `media_admin` | 手动清理单条缓存 |
| `GET /stream/v1/{track_ref}?ticket=` | 持票 | 统一出流；支持 HTTP Range |

## 7. 错误码

| code | 含义 |
|---|---|
| `unauthorized` | 未认证 / 会话失效 |
| `forbidden` | 角色权限不足 |
| `bad_request` | 参数非法（含 track_ref 格式错误、未进房就发房间操作） |
| `queue_full` | 队列达到房间 `max_queue` 上限 |
| `quota_exceeded` | 超出身份的点歌限额（policy.queue_limits） |
| `not_found` | 房间 / 条目 / 曲目不存在 |
| `provider_error` | Provider 调用失败（附 message） |
| `internal` | 服务端内部错误 |
| `rate_limited` | 预留 |

## 8. 明确不做的（当前版本）

- 投票切歌、DJ 模式（policy_json 可扩展，不实现逻辑）
- 播放进度持久化（重启后当前曲目丢失；队列保留在 DB，房间启动时自动续播队首）
- 队列事件的增量 diff 下发
- OIDC / 账号密码登录
- 凭据加密存储（当前明文存 credentials 表；加密需引入密钥管理，随凭据种类增多再做）

## 9. 客户端实现清单

一个新客户端（播放代理、WebUI、游戏 Mod）的最小正确实现：

### 9.1 连接时序

```
1. 连接 WebSocket /ws/v1
2. 校时：3–5 轮 ping/pong（§2.1），得 offset
3. auth（§3.1）→ 拿到 identity
4. room.join（§4.2）→ 等 room.joined
5. 接收快照三件套（§4.1），初始化本地状态
6. 进入事件循环：playback/queue/listeners.changed 覆盖本地副本
```

### 9.2 播放渲染（仅收听型客户端）

```
每次 playback.changed：
  current 为 null          → 停止播放
  track_ref 变了           → 用 stream_url 加载新媒体
  playing 变了             → 同步暂停/恢复
  之后执行校偏：
    should_be = position_ms + (server_now() - updated_at) * rate  (playing)
    drift = 本地播放位置 - should_be
    |drift| > 150ms        → seek 到 should_be
    30–150ms               → 微调播放速率 (0.98–1.02) 缓追
    < 30ms                 → 不动
周期任务：每 1s 校偏一次；每 60s 重校时一次（§2.2）
```

### 9.3 拉流

- `GET {server}{stream_url}`（stream_url 已含 ticket 与版本前缀，直接拼接服务器基址即可）。
- 支持 Range；票据 5 分钟内可复用，曲目播完即失效。
- 401 = 票据失效/缺失：等待下一条 `playback.changed` 获取新 `stream_url`，不要自行编造。

### 9.4 断线与重连

- 服务端会话与房间成员关系都是**内存态**：连接断开即离房，身份作废。
- 重连后 MUST 重新执行 9.1 全部四步（校时 → auth → join → 快照）。
- 服务端会主动断开发送缓冲满的慢客户端；客户端实现 SHOULD 指数退避重连（如 1s/2s/5s/10s 封顶）。

### 9.5 其他纪律

- MUST 忽略 JSON 中未知字段（版本纪律，§0）。
- MUST 用 `ref` 匹配请求与响应（§4.3）；广播消息无 `ref`。
- SHOULD 假定任何操作都可能收到 `error` 响应（错误码见 §7），按 code 而非 message 分支。
- 服务器位置推算的唯一依据是 playback 五元组 + offset（§2.2）；不要信任本地计时外推超过校偏间隔。

### 9.6 参考实现

仓库 `internal/client/` 是本协议的 Go 参考实现；`cmd/yuzu-agent/`（MPV 渲染代理）与 `cmd/yuzu-cli/`（控制端）是两类客户端形态的最小完整示例。
