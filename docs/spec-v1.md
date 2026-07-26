# Yuzu Jukebox 协议与房间状态机规格 v1

本文档是客户端与服务端对接的唯一权威规格。实现与本文冲突时，以本文为准。

## 0. 总则

- 服务端是唯一权威状态源；客户端负责展示、输入解析、平台适配与播放渲染，不在服务端复制领域状态机。
- **Server/Client 边界**：Server 只接收本规格定义的领域请求并返回领域 JSON；它不解析 IM/游戏聊天命令，不决定命令前缀，不生成聊天文案或平台组件。命令解析、国际化、消息排版、按钮/卡片和游戏内 UI 均属于 Client。
- 官方 WebUI、CLI、`yuzu-agent` 与第三方 AstrBot Plugin、Game Mod 在架构上都是 Client：

| Client 类型 | 主要职责 |
|---|---|
| WebUI | 交互展示、标准 REST/WS 控制，可选本地播放 |
| CLI | 终端命令解析、领域 JSON 展示与管理操作 |
| Agent | 无头播放渲染；需要远程管理时实现 player plane |
| Integration Client（AstrBot Plugin、Game Mod bridge 等） | 把外部 scope/subject 解析为 Yuzu actor，把 IM/游戏输入翻译成标准 REST/WS，再把领域 JSON 渲染回外部平台 |

- 协议面及边界如下；**不存在 `/im/v1`**，也不存在 AstrBot/Game Mod 专用的服务端命令协议：

| 协议面 | 入口 | 用途边界 |
|---|---|---|
| REST | `/api/v1/*` | 无状态领域查询/命令、认证和低频管理；请求结束后不建立房间订阅 |
| WebSocket | `/ws/v1` | 校时、认证、加入房间、实时全量快照广播；也可发送与 REST 共用领域服务的房间命令 |
| stream | `/stream/v1/{track_ref}` | 持短期票据获取媒体字节与 HTTP Range；不承载领域命令 |
| player plane | `/ws/v1` 的 `player.*` + `/api/v1/players*` | 管理无头播放端的音量、静音与换房；不供普通交互客户端或 Integration 命令复用 |

- 所有时间戳为 **Unix 毫秒（UTC）**。
- 所有端点路径携带主版本号。版本纪律：
  - **只有破坏性变更才升版本**（删字段、改字段类型/语义、改鉴权方式、改状态机行为）。
  - 新增字段、消息类型、端点不算破坏性，直接在现版本内添加。
  - 客户端实现 MUST 忽略响应 JSON 中的未知字段。
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
  - ≤ 150ms → 不动
  - **输出延迟区分**：播放器位置读数可能系统性少报（蓝牙等输出链路延迟被播放器从读数中扣除，典型 200–300ms；MPV time-pos 与蓝牙输出下的浏览器 HTMLAudioElement 均有实测）。推荐处理（yuzu-agent 与 WebUI 一致）：
    - (a) 开播即定位（loadfile 带 `start` / 元数据就绪即 seek 至 should_be），避免"从 0 播一秒再大跳"；
    - (b) 对齐 seek 生效后的首个**有效**漂移样本即为该偏差，学为基线 `drift_baseline`，此后只纠正超出基线的变化——校准开销为一次 seek，而非反复拽恒定读数差造成的 seek 风暴。细则：
      - 基线为每曲目状态：换曲目时清零并重新进入待学习；开播定位（a）即首次对齐 seek；
      - 学习样本须在 seek 落定且缓冲健康时采集（Web：`!seeking && readyState >= HAVE_FUTURE_DATA`；MPV：seek 完成且未 buffering），否则暂缓学习——缓冲冻结期的读数不是延迟偏差；
      - 待学习期间 |样本| > 1s 视为初始定位未生效而非输出延迟：先按无基线对齐一次，保持待学习；
      - 纠正性 seek 目标为 `should_be + drift_baseline`（补偿读数差，听觉位置一次到位）；该 seek 生效后重新学习基线，残余抖动被基线吸收而非触发新 seek。

### 2.3 TrackRef 扩展（provider 自定义 id 段）

`provider:id` 的 id 段格式由 provider 自定义，服务端其它层一律视为不透明字符串：

- `ncm:<song_id>`
- `bili:<bvid>`、`bili:<bvid>?p=<N>`（分 P；无 ?p 为第 1 P。合集语义：标题 = 分 P 名，Album = 视频总标题）
- `local:<媒体id>`

含特殊字符的 ref 嵌入 URL 路径时 MUST 做 PathEscape（stream_url / cover_url 已由服务端转义）。

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

- `password` 字段是**全局管理员口令**（可选）：不传或错误时签发普通 guest Principal（`g_` + `sha256("guest:"+name)` 前 12 hex，`kind=guest`，roles 为 `listener+requester`）；命中 `admin_password` 时签发独立 password Principal（`p_` + `sha256("password:"+name)` 前 12 hex，`kind=password`），追加 `room_admin+media_admin`。两种 Principal 即使显示名相同也绝不共享身份或角色。
- **房间访客密码不在此处传递**——它是每房间一个的进门凭证，在 `room.join` 时校验（见 4.2）。
- `session_token` 用于 REST 通道鉴权（见 §6）；WS 通道上 `auth.ok` 之后的操作直接用该连接的身份。
- 普通 guest 身份 ID 由名字确定性派生，同名重连仍是同一人；管理员口令身份使用上述独立 `password` 命名空间。
- 普通会话默认 TTL 24h，**持久化于 sessions 表**：重启不失效，过期自动清理；`DELETE /api/v1/auth/session` 吊销。Integration actor 会话例外，固定 TTL 5 分钟（见 3.6）。
- 授权不以 `kind` 判断；以当前 Principal 的 `roles` 与 Room grant 为准（见 3.4）。`kind` 仅可参与房间队列限额策略。

### 3.2 会话 token 认证（OIDC 等 REST 登录后的 WS 接入）

```json
→ { "type": "auth", "ref": "1", "data": { "session_token": "..." } }
← { "type": "auth.ok", ... }   // 与 3.1 同构
```

凡经 REST 获得的 session_token（guest REST 登录、OIDC 登录）都可直接在 WS 上换取连接身份。

### 3.3 OIDC 登录（Zitadel 等 IdP）

```
POST /api/v1/auth/oidc { "id_token": "<IdP 签发的 ID token>", "access_token": "<可选>" }
→ { "identity": { "id": "o_…", "name": "<preferred_username>", "kind": "oidc",
                  "roles": ["listener", "requester", …映射结果] },
    "session_token": "..." }
```

- 服务端始终验签并校验 **ID token**（恒为 JWT，RS256）。可选 access token（Zitadel 默认 opaque）只用于调用 userinfo 补显示名/角色；成功取得的 userinfo 必须带有与已验证 ID token 严格相同的 `sub`，缺失或不匹配时拒绝登录。
- 验签材料：`{issuer}/.well-known/openid-configuration` → `jwks_uri` → JWKS 缓存；未知 kid 自动刷新一次（密钥轮换）。校验 iss / aud（命中 config `oidc.client_id` 或 `oidc.extra_client_ids` 任一）/ exp（60s 宽限）。
- 显示名优先取 ID token 的 `preferred_username`；缺失时可用上述同主体 userinfo 的 `preferred_username`/`name` 补充，最终回退稳定 `sub`。
- 身份 ID：`o_` + sha256(sub) 前 12 hex——`sub` 在 IdP 里永不变，改名不影响归属。
- 角色映射：扫描 payload 中所有 `urn:zitadel:iam:org:project*:roles` claim，对象 key 即角色名；config `oidc.role_mapping` 把 Zitadel 角色名映射为 yuzu roles。未命中者保持 `listener + requester`。
- IdP 侧前提（Zitadel console）：Native 类型应用。角色进 ID token 需 **Application 级**设置（Token Settings 中勾选包含 User Roles 的选项，旧版 UI 叫 "User Roles Inside ID Token"，新版与 Project 级同名为 "Assert Roles on Authentication"，API 字段 `id_token_role_assertion`）——这是角色映射的主路径。Project 级 "Assert Roles on Authentication" 只让 roles 出现在 userinfo，为可选兜底（客户端传 access_token 时服务端会合并 userinfo 角色）。替代方案：客户端在授权请求中携带 scope `urn:zitadel:iam:org:projects:roles`，可不依赖上述 console 设置让 roles 进 token。
- 客户端获取 id_token 的方式服务端不感知：CLI/agent 推荐 Device Authorization Grant；WebUI 推荐 Authorization Code + PKCE。

### 3.4 Principal、角色与当前状态授权

`Principal` 是持久主体，当前记录包含 `id/name/kind/roles/active`（OIDC 主体另存 `oidc_subject`）。对外的 `identity` JSON 仍只有 `id/name/kind/roles`；`active` 是服务端授权状态。

| role / capability | 当前语义 |
|---|---|
| `listener` | 基础收听 role；大厅目录、历史、统计与歌词要求此 role；WS 加房和无状态 state 查询只要求已认证，并不另查它 |
| `requester` | 点歌以及搜索、Provider/歌单读取等 requester 操作 |
| `room_admin` | 全局 Room 管理员；是所有 Room 的 controller，并可管理 Integration 映射与 Room grant |
| `media_admin` | Provider 凭据、本地媒体、缓存与歌单写操作 |
| Room grant `controller` | 不是 role；只让指定 Principal 控制指定 Room |

Room 控制授权的完整判定是：

```
controller(room, principal)
  = principal.roles 包含 "room_admin"
    OR 存在 (room_id, principal_id, "controller") grant
```

`queue.remove` 另保留所有者路径：条目的 `requested_by == principal.id` 时可移除自己的待播条目，不要求 controller；移除他人条目才要求 controller。

- session 行内保存的 identity 是**签发时快照，不是后续授权源**。每次 REST Bearer 认证及每次 WS `auth {session_token}` 握手都会重读当前 Principal；当前 `roles/name/kind` 覆盖 session 快照。
- Principal 不存在或 `active=false` 时，已有普通 session 视为无效：REST 返回 HTTP 401 `unauthorized`，WS 认证返回 `error/unauthorized`。
- Integration actor resolve 若命中 disabled/missing 的链接 Principal，或其自动生成的 guest Principal 已 disabled，则不签发 token，返回 HTTP 403 `forbidden`。
- 已完成 `auth` 的 WS 连接持有握手时身份；Principal 后续变更不会主动踢掉该连接。REST 在下一请求立即采用新状态，WS 在重新认证/重连后采用新状态。

### 3.5 Integration credential

可信 Integration 由服务端配置文件根级 `integrations` 静态配置：

```json
{
  "integrations": [
    { "id": "astrbot-main", "token": "<高熵静态密钥>" }
  ]
}
```

- 配置形状严格为 `{id, token}`；两者均不得为空，`id` 与 `token` 各自不得重复，否则服务端启动失败。
- Integration token 只证明“哪个可信 Integration 正在请求 actor resolve”；**它本身没有 Yuzu identity、role 或 Room grant**。
- Integration token 只用于 `POST /api/v1/integrations/actors/resolve`。把它用于房间 REST、管理 REST 或 WS session 认证会得到 `unauthorized`。

### 3.6 Integration actor resolve

Integration Client 收到外部事件后，先用 Integration token 解析事件的实际操作者：

```http
POST /api/v1/integrations/actors/resolve
Authorization: Bearer <integration_token>
Content-Type: application/json
```

```json
{
  "adapter_id": "astrbot",
  "scope": { "type": "group", "id": "123456" },
  "subject": { "id": "9988", "display_name": "阿柚" }
}
```

所有五个字符串 `adapter_id/scope.type/scope.id/subject.id/subject.display_name` 均为必填非空值。成功返回：

```json
{
  "identity": {
    "id": "ig_…",
    "name": "阿柚",
    "kind": "guest",
    "roles": ["listener", "requester"]
  },
  "default_room_id": "lobby",
  "actor_token": "<普通 Yuzu session token>",
  "expires_at": 1720000300000
}
```

- `actor_token` 是标准 session token，可直接用于 `Authorization: Bearer <actor_token>`，也可作为 WS `auth` 的 `session_token`；固定有效期 **5 分钟**。`expires_at` 是与实际会话过期点相同的 Unix 毫秒。
- `default_room_id` 来自 external scope 绑定；未绑定时该字段**省略**，不是空值错误。它只是 Client 的默认选择，不授予权限，也不替代 WS `room.join` 的房间密码。
- `integration_id` 从通过认证的 Integration token 得出，请求体不能冒充。external scope key 为
  `(integration_id, adapter_id, scope.type, scope.id)`；external subject key 再追加 `subject.id`。`display_name` 不参与 key：未链接的 synthetic guest 在下次 resolve 时更新名称；已链接时忽略它并以当前 Principal 名称为准。
- 若 external subject key 已链接到一个 Principal，返回该 Principal 的**当前** identity/roles；若未链接，则生成稳定 guest：ID 为 `ig_` + `sha256(JSON([integration_id, adapter_id, scope.type, scope.id, subject.id]))` 的完整十六进制，`kind=guest`，roles 为 `listener+requester`。Client MUST 把返回的 `identity.id` 当作不透明值。
- scope 绑定、subject 链接和 controller grant 的管理合同见 6.4。

## 4. 房间会话

### 4.1 房间快照

`room.join` 成功时，服务端按固定顺序推送五条消息：

1. `room.joined`（回带 ref，data: `{"room_id": "..."}`）
2. `playback.changed` —— 当前播放状态
3. `queue.changed` —— 完整队列
4. `radio.changed` —— 电台模式状态（见 4.5）
5. `listeners.changed` —— 完整听众列表

四条状态快照此后有任何变更都会再次全量推送，客户端始终用最新一份覆盖本地副本。

**`playback.changed` 的 data（即 playback 对象本身，无外层包装）：**

```json
{
  "current": {
    "entry_id": "e_991", "track_ref": "ncm:347230",
    "title": "海阔天空", "artist": "Beyond", "duration_ms": 326000,
    "album": "乐与怒", "cover_url": "/api/v1/cover/ncm:347230",
    "source_url": "https://music.163.com/song?id=347230",
    "contributors": [{"role": "artist", "name": "Beyond"}],
    "size_bytes": 8172890, "bitrate_kbps": 320,
    "requested_by": "g_8f3k2", "requester_name": "小柚", "added_at": 1720000000000,
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

**`queue.changed` 的 data：**`{"queue": [ <队列条目>, ... ]}`——条目字段同 `current`（但无 `stream_url`/`size_bytes`/`bitrate_kbps`）。

**曲目元数据层次**（客户端只需面对这一个形状，字段可空即降级）：

- 曲目层（入队快照，队列与播放广播都带）：`title/artist/duration_ms/album/cover_url/source_url/contributors/requester_name`。`requester_name` 是入队时操作者 identity 的显示名快照，请求人后来改名不影响历史条目；旧数据为空串时客户端可降级显示 `requested_by`。`cover_url` 一律为服务端代理路径 `/api/v1/cover/{track_ref}`（源站可能需 Referer）。
- 物理层（仅 `playback.current`，Resolve/缓存后可得）：`size_bytes/bitrate_kbps`。
- provider 能力缺席合法：bili 无歌词、local 无 source_url，字段缺省即降级。

**`listeners.changed` 的 data：**`{"listeners": [ {"id": "...", "name": "..."}, ... ]}`

**`radio.changed` 的 data：**`{"radio": null | {"source": "...", "description": "...", "finite": true, "shuffle": false, "once": false}}`（见 4.5）

### 4.2 客户端 → 服务端消息

| type | data | 权限 | 说明 |
|---|---|---|---|
| `room.join` | `{room_id, password}` | 任何已认证身份 | 密码错误返回 `error` |
| `room.leave` | `{room_id}` | — | |
| `queue.add` | `{room_id, track_ref}` 或 `{room_id, track_refs[1..100]}` | `requester` | 队尾追加；批量为原子：整体预校验，任一失败一条不加；ack 回 entry_ids |
| `queue.remove` | `{room_id, entry_id}` | 条目所有者 / 该 Room 的 controller | |
| `queue.move` | `{room_id, entry_id, to_index}` | 该 Room 的 controller | |
| `playback.pause` / `playback.resume` | `{room_id}` | 该 Room 的 controller | |
| `playback.seek` | `{room_id, position_ms}` | 该 Room 的 controller | |
| `playback.skip` | `{room_id}` | 该 Room 的 controller | 切下一首 |
| `radio.play` | `{room_id, source, shuffle, once}` | 该 Room 的 controller | 进入电台模式（见 4.5） |
| `radio.stop` | `{room_id}` | 该 Room 的 controller | 退出电台模式 |

### 4.3 服务端 → 客户端消息

| type | 时机 | data 要点 |
|---|---|---|
| `ack` | 请求处理成功 | 空对象；**回带 `ref`**；`queue.add` 时回 `{"entry_ids": [...]}` |
| `error` | 请求失败 | `{code, message}`；**回带 `ref`** |
| `room.joined` | `room.join` 成功 | `{"room_id"}`；回带 `ref` |
| `room.left` | `room.leave` 成功 | 空对象；回带 `ref` |
| `pong` | 响应 `ping` | `{client_time, server_time}`；回带 `ref` |
| `playback.changed` | 播放/暂停/seek/切歌/自然结束/新听众进房 | 完整 `playback` 对象（见 4.1） |
| `queue.changed` | 队列任何变更 | `{"queue": [...]}`（当前版本全量下发，量大后再做 diff） |
| `listeners.changed` | 听众进出 | `{"listeners": [...]}` |
| `radio.changed` | 电台开启/停止/新听众进房 | `{"radio": null | {...}}` |

**请求-响应匹配**：在房间协议中，除广播四件套（`playback/queue/radio/listeners.changed`）外，响应都回带触发请求的 `ref`。客户端 MUST 用 `ref` 路由响应；无匹配 `ref` 的消息按广播处理。player plane 的异步 `player.command` 是独立的无 `ref` 服务端推送（见 4.8）。

**关键设计：`playback.changed` 永远携带完整 playback 对象。** 客户端无需区分事件原因，统一按"以最新状态重算 should_be"处理。新增听众也凭此一次性对齐。

### 4.4 WS 与无状态 REST 的操作边界

- WS 房间命令要求连接先以 `room.join` 加入同一个 Room；`room.join` 会检查房间访客密码，并建立实时广播订阅。
- 6.3 的 Room REST 不建立 listener、不要求先 join，也不接收或检查房间访客密码；它只校验标准 Bearer session 及各操作的 requester/controller 权限。
- 两种传输适配器调用同一个 Room 领域服务和同一个 actor。REST 命令造成的状态变化会照常广播给已加入该 Room 的 WS 客户端；WS 命令也可由后续 REST state 查询观察。
- `GET .../state` 是一次性、按调用身份投影的完整快照，不是订阅。需要实时变化时仍 MUST 使用 `/ws/v1` 加房并处理四类 `*.changed` 广播。

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
- 普通 guest 身份 ID 由名字确定性派生（`g_` + `sha256("guest:"+name)` 前 12 hex），同名重连仍是同一人；Integration synthetic guest 使用 3.6 的完整 external subject key——两者的限额与“移除自己点的歌”均可跨会话成立。
- 电台补充不经过限额检查（补充只在队列 <3 时触发，天然有界）。
- 播放历史与统计见 6 节 REST；`play_history` 只记**播放**（结束原因 `finished`/`skipped`），不记点歌意图。

### 4.8 播放端管理平面（player plane）

无头渲染端（嵌入式播放器）可注册为**可管理播放端**，管理员远程调节，无需 SSH：

```
agent: → {"type": "player.hello", "data": {"device": "speaker-01", "caps": ["volume","mute","join_room"]}}
       ← {"type": "player.hello.ok", "data": {"player_id": "c_…"}}
agent: → {"type": "player.state", "data": {"volume": 42, "muted": false}}   // 变更时上报
server:← {"type": "player.command", "data": {"op": "set_volume", "value": 42}}  // 服务端下发
```

- 注册要求已认证身份；连接断开自动注销。
- **换房间不收房间密码**：`join_room` 由服务端直接迁移连接（房间 actor 自动推送快照，agent 原样重渲染）。
- 管理入口是 REST：`GET /api/v1/players`、`POST /api/v1/players/{id}/command {op, value}`（op: `set_volume` 0-100 / `set_mute` bool / `join_room` 房间 id），均需 `room_admin`。
- 面向用户的交互式客户端 SHOULD NOT 实现该平面（本地 UI 自己管音量）。

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

## 6. REST API（`/api/v1`）

### 6.1 共通合同

- 端点权限以 6.2 为准。受保护的标准 REST 使用 `Authorization: Bearer <session_token>`，token 可来自 guest/OIDC 登录或 3.6 的 `actor_token`；Integration token 不是标准 session。guest/OIDC/config 与 cover 是公开端点，actor resolve 改用 Integration token，logout 只要求非空 Bearer 值，stream 改用查询参数 ticket。
- Server 返回领域 JSON，不返回已经排版的聊天文本。Integration Client 自行把结果转换成 AstrBot 消息链、游戏 UI、终端文本等。
- 客户端 MUST 忽略**响应**中的未知字段。6.3 中带 JSON body 的 Room 控制端点与 6.4 的 Integration 端点采用严格 JSON 解码：未知字段、类型不符、多个 JSON 值均返回 HTTP 400 `bad_request`；Room 控制 JSON 解码器最多读取 1 MiB。
- 当前 v1 **没有 `Idempotency-Key` 合同或去重存储**。不得虚构该请求头的语义；尤其 `POST .../queue` 在响应丢失后的盲目重试可能重复入队。
- REST 错误统一使用以下 envelope，客户端按 `error.code` 分支，不按 `message` 分支：

```json
{
  "error": {
    "code": "forbidden",
    "message": "controller capability required"
  }
}
```

### 6.2 端点目录

**认证、Integration、Room 与 player：**

| 端点 | 权限 | 说明 |
|---|---|---|
| `POST /api/v1/auth/guest` | — | 访客认证，body `{"name", "password"}`（password 为全局管理员口令，可选）；返回 `{identity, session_token}` |
| `POST /api/v1/auth/oidc` | — | OIDC 认证，body `{"id_token","access_token"?}`；返回 `{identity, session_token}`；未启用 404 |
| `GET /api/v1/auth/oidc/config` | — | 公开 OIDC 配置：issuer / client_id / client_ids（含 extra_client_ids）；未启用 404 |
| `DELETE /api/v1/auth/session` | 非空 Bearer token | 按所给 token 吊销会话（即使已失效也返回成功）；缺 Authorization 时 401 |
| `GET /api/v1/integrations` | `room_admin` | 已配置 Integration 的公开 ID 列表；绝不返回 token |
| `POST /api/v1/integrations/actors/resolve` | Integration token | external scope/subject → 5 分钟标准 actor session（见 3.6、6.4） |
| `GET /api/v1/integrations/{id}/scopes` | `room_admin` | 该 Integration 的全部 external scope→Room 绑定，稳定排序 |
| `PUT/DELETE /api/v1/integrations/{id}/scopes` | `room_admin` | 绑定/解绑 external scope 的默认 Room |
| `GET /api/v1/integrations/{id}/subjects` | `room_admin` | 该 Integration 的全部 external subject→Principal 链接，稳定排序 |
| `PUT/DELETE /api/v1/integrations/{id}/subjects` | `room_admin` | 链接/解绑 external subject 与 Principal |
| `GET /api/v1/principals?q=&limit=` | `room_admin` | 按 ID 或名称可选搜索 Principal；默认/上限 100，稳定排序；不返回 OIDC subject |
| `GET /api/v1/rooms` | `listener` | 房间目录；每项在 `id/name/policy` 外含实时 `listener_count`，以及空闲为 `null`、播放或暂停时为 `{title,artist,duration_ms,cover_url,position_ms,updated_at,playing,rate}` 的 `now_playing`；绝不含 `stream_url` |
| `POST /api/v1/rooms` / `PATCH /api/v1/rooms/{id}` | `room_admin` | 建/改房间（名称、访客密码、policy） |
| `DELETE /api/v1/rooms/{id}` | `room_admin` | 删除房间（队列与历史级联清理） |
| `GET /api/v1/rooms/{id}/grants` | `room_admin` | 该 Room 的全部显式 `controller` grants，稳定排序 |
| `PUT/DELETE /api/v1/rooms/{id}/grants/{principal_id}` | `room_admin` | 授予/撤销该 Principal 的 Room `controller` capability |
| `GET /api/v1/rooms/{id}/history?offset=&limit=` | `listener` | 播放历史，最新在前（默认 50，上限 200） |
| `GET /api/v1/rooms/{id}/stats?limit=` | `listener` | 曲目热度榜（默认 20，上限 100） |
| `GET /api/v1/rooms/{id}/capabilities` | 标准 session | 当前身份的有效 Room capability；Room 不存在为 404 |
| `GET /api/v1/rooms/{id}/state` | 标准 session | 无副作用完整状态快照 |
| `POST /api/v1/rooms/{id}/queue` | `requester` | 单条或 1–100 条原子入队 |
| `DELETE /api/v1/rooms/{id}/queue/{entry_id}` | 所有者 / controller | 移除待播条目 |
| `PATCH /api/v1/rooms/{id}/queue/{entry_id}` | controller | 移动待播条目 |
| `POST /api/v1/rooms/{id}/playback/{op}` | controller | `pause/resume/skip/seek` |
| `POST /api/v1/rooms/{id}/radio` / `DELETE /api/v1/rooms/{id}/radio` | controller | 开启/停止电台 |
| `GET /api/v1/players` | `room_admin` | 在线播放端清单 |
| `POST /api/v1/players/{id}/command` | `room_admin` | 播放端指令：set_volume / set_mute / join_room |

**Provider、歌单、媒体与出流：**

| 端点 | 权限 | 说明 |
|---|---|---|
| `GET /api/v1/search?provider=&q=` | `requester` | 转发 Provider.Search，返回 Track 列表（含 track_ref） |
| `GET /api/v1/providers` | `requester` | 已注册的 Provider 列表 |
| `POST /api/v1/providers/{id}/credential` | `media_admin` | body `{"payload":"..."}`；先校验再热生效，凭据存储于服务端且永不下发 |
| `POST /api/v1/providers/{id}/qrlogin` | `media_admin` | 返回 `{key, qr_content}`，Client 自行渲染二维码 |
| `GET /api/v1/providers/{id}/qrlogin/{key}` | `media_admin` | 返回 waiting / scanned / ok / expired；ok 时凭据已在服务端生效 |
| `GET /api/v1/playlists` | `requester` | 歌单列表（含曲目数，不含条目） |
| `POST /api/v1/playlists` | `media_admin` | 创建歌单 `{name, description}` |
| `GET /api/v1/playlists/{id}?offset=&limit=` | `requester` | 歌单详情 + 条目分页（默认 50，上限 200） |
| `DELETE /api/v1/playlists/{id}` | `media_admin` | 删除歌单 |
| `POST /api/v1/playlists/{id}/items` | `media_admin` | 追加 `{track_refs:[...]}`（单次≤100） |
| `DELETE /api/v1/playlists/{id}/items/{ord}` | `media_admin` | 按序号删除，后续重排 |
| `PATCH /api/v1/playlists/{id}/items/{ord}` | `media_admin` | body `{to_ord}`；目标 clamp 到 `[1,len]` |
| `POST /api/v1/playlists/import` | `media_admin` | `{provider,playlist_id}`（外部歌单；ncm 支持 ID/URL）或 `{source}`（曲目源物化）二选一，可选 `{name}` |
| `GET /api/v1/media` | `media_admin` | 按 `created_at` 倒序返回 `track_ref/title/artist/duration_ms/size_bytes/uploaded_by/created_at`；空列表为 `{"media":[]}` |
| `DELETE /api/v1/media/{ref}` | `media_admin` | 仅接受 `local:` ref；删除媒体行、文件与对应缓存，不级联队列/歌单/历史引用；旧引用以后 Resolve 失败 |
| `POST /api/v1/media/upload` | `media_admin` | local provider 上传 |
| `GET /api/v1/media/cache` | `media_admin` | 返回 `entries`、进行中的 `downloads`、内存态最近 20 条 `history`、`total_bytes` 与 `max_bytes` |
| `DELETE /api/v1/media/cache/{track_ref}` | `media_admin` | 手动清理单条缓存 |
| `POST /api/v1/media/cache/prune` | `media_admin` | `{unused_days:N}`（非负整数，`N=0` 清空）；跳过下载中条目，返回 `{evicted,freed_bytes}` |
| `GET /api/v1/cover/{track_ref}` | — | 公开统一封面代理，Cache-Control 1 天 |
| `GET /api/v1/lyrics?track_ref=` | `listener` | `{type:"lrc",lrc,tlrc?}`；Provider 无能力时 501 `not_supported` |
| `GET /stream/v1/{track_ref}?ticket=` | 持票 | 统一出流；支持 HTTP Range |

配置 `cache_auto_prune_days` 控制自动按龄清理：默认 `0`（关闭）；正整数时每 6 小时清理超过该天数未访问的缓存，正在下载的条目跳过。

### 6.3 无状态 Room 合同（合同 A）

这些路径都不建立 WS 连接或 Room listener，不要求 `room.join`，也不检查房间访客密码。`controller` 的定义是全局 `room_admin` **OR** 此 Principal 在此 Room 的 `controller` grant（3.4）。

**状态响应：**

`GET /api/v1/rooms/{id}/capabilities` 对任意有效标准 session 返回当前身份的有效 capability：

```json
{
  "capabilities": {
    "controller": true
  }
}
```

`controller` 必须由 3.4 的统一 Authorizer 判定，Client 不得只看自身 roles 或自行缓存 grant 推导结果。查询不建立 listener；Room 不存在返回 404 `not_found`。

`GET /api/v1/rooms/{id}/state` 对任意有效标准 session 返回 HTTP 200：

```json
{
  "playback": { "current": null, "position_ms": 0, "updated_at": 0, "playing": false, "rate": 1 },
  "queue": [],
  "radio": null,
  "listeners": []
}
```

- 四个字段的完整形状与 4.1 的 WS 快照相同；区别是 REST 顶层直接使用 `queue` 数组与 `radio` 值，没有 `queue.changed`/`radio.changed` 的 data 包装。
- `playback.current` 非空时包含按本次调用 identity 签发的 `stream_url`；队列条目仍不含 `stream_url`。
- `listeners` 只反映已加入的 WS listener。查询本身无副作用，重复查询不会把调用者加入列表。

**命令请求与成功响应：**

| 方法与路径 | JSON body | 权限 | HTTP 200 body |
|---|---|---|---|
| `POST /api/v1/rooms/{id}/queue` | `{"track_ref":"ncm:..."}` 或 `{"track_refs":["...", ...]}`，严格二选一；数组 1–100 | `requester` | `{"entry_ids":["e_...", ...]}`，顺序与请求一致 |
| `DELETE /api/v1/rooms/{id}/queue/{entry_id}` | 无 | 条目所有者 / controller | `{}` |
| `PATCH /api/v1/rooms/{id}/queue/{entry_id}` | `{"to_index":0}`；零基、非负且须落在当前待播队列范围 | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/pause` | 无（空对象也可） | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/resume` | 无（空对象也可） | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/skip` | 无（空对象也可） | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/seek` | `{"position_ms":1234}`；非负整数，超过时长时 clamp | controller | `{}` |
| `POST /api/v1/rooms/{id}/radio` | `{"source":"playlist:...","shuffle":false,"once":false}` | controller | `{}` |
| `DELETE /api/v1/rooms/{id}/radio` | 无 | controller | `{}` |

- 单条与批量入队都会先解析全部 Track，再由 Room actor 整批预校验并追加；任一 ref、Provider、总量或身份限额失败时一条不加。
- 每个成功命令都经与 WS 相同的领域服务进入同一个 Room actor；其状态变化随后广播给该 Room 的 WS listeners。
- `{id}`、`{entry_id}`、`{track_ref}` 嵌入 URL 时按路径段转义；不要把 `bili:...?p=...` 的 `?` 当作 URL 查询起点。

合同 A 的错误映射：

| HTTP / `error.code` | 条件 |
|---|---|
| 400 / `bad_request` | JSON/字段非法、未知 op、非法 ref/source、`to_index` 越界 |
| 401 / `unauthorized` | session 缺失、过期、吊销，或 Principal disabled/不存在 |
| 403 / `forbidden` | 缺 requester/controller，或非所有者尝试移除条目 |
| 404 / `not_found` | Room、待播 entry 不存在或 Room actor 已关闭 |
| 409 / `conflict` | 队列总量/身份限额，或 pause/resume/seek 没有可用当前播放、当前状态不允许该迁移 |
| 502 / `provider_error` | 入队取 Track 时 Provider 失败 |
| 500 / `internal` | 未分类的服务端错误 |

注意：WS 对队列上限仍使用 `queue_full` / `quota_exceeded`；当前合同 A 将这两类状态冲突统一映射为 HTTP 409 `conflict`。

### 6.4 Integration 映射与 Room grant 管理

actor resolve 使用 Integration token；本节其余管理端点反而必须使用具有当前 `room_admin` role 的**标准 session**。Integration token 本身调用这些端点会是 401，普通 actor token 默认只有 listener/requester，调用会是 403。

**Resolve：**

- `POST /api/v1/integrations/actors/resolve` 的严格请求/响应形状见 3.6；成功为 HTTP 200。
- Integration token 缺失/错误为 401 `unauthorized`；字段缺失、空白、类型错误或未知字段为 400 `bad_request`；链接/guest Principal disabled 为 403 `forbidden`。

**管理查询：**

| 方法与路径 | HTTP 200 JSON 示例 |
|---|---|
| `GET /api/v1/integrations` | `{"integrations":[{"id":"generic-bridge"}]}` |
| `GET /api/v1/integrations/{id}/scopes` | `{"scopes":[{"integration_id":"generic-bridge","adapter_id":"onebot","scope_type":"group","scope_id":"42","room_id":"main"}]}` |
| `GET /api/v1/integrations/{id}/subjects` | `{"subjects":[{"integration_id":"generic-bridge","adapter_id":"onebot","scope_type":"group","scope_id":"42","subject_id":"7","principal_id":"p_123"}]}` |
| `GET /api/v1/principals?q=&limit=` | `{"principals":[{"id":"p_123","name":"Alice","kind":"oidc","roles":["listener"],"active":true}]}` |
| `GET /api/v1/rooms/{id}/grants` | `{"grants":[{"room_id":"main","principal_id":"p_123","capability":"controller"}]}` |

所有列表均确定排序且空结果返回 `[]`。Integration 列表绝不含 token；Principal 列表只公开 `id/name/kind/roles/active`，绝不含 `oidc_subject`。`q` 可省略，按 ID 或名称匹配；`limit` 默认 100 且最大 100。Integration scope/subject 的 `{id}` 未配置时、grant 的 Room 不存在时返回 404 `not_found`。

**管理请求：**

| 方法与路径 | 严格 JSON body | 成功语义与 HTTP 200 body |
|---|---|---|
| `PUT /api/v1/integrations/{id}/scopes` | `{adapter_id,scope_type,scope_id,room_id}` | upsert scope→Room；`{"scope":{integration_id,adapter_id,scope_type,scope_id,room_id}}` |
| `DELETE /api/v1/integrations/{id}/scopes` | 与 PUT 相同 | 仅在 `room_id` 与当前绑定一致时删除；`{"ok":true}` |
| `PUT /api/v1/integrations/{id}/subjects` | `{adapter_id,scope_type,scope_id,subject_id,principal_id}` | upsert subject→Principal；`{"subject":{integration_id,adapter_id,scope_type,scope_id,subject_id,principal_id}}` |
| `DELETE /api/v1/integrations/{id}/subjects` | 与 PUT 相同 | 仅在 `principal_id` 与当前链接一致时删除；`{"ok":true}` |
| `PUT /api/v1/rooms/{id}/grants/{principal_id}` | `{room_id,principal_id,"capability":"controller"}` | upsert grant；`{"grant":{room_id,principal_id,capability}}` |
| `DELETE /api/v1/rooms/{id}/grants/{principal_id}` | 与 PUT 相同 | 撤销存在的 grant；`{"ok":true}` |

- Integration 路径 `{id}` 必须是服务端配置的 Integration；否则 404 `not_found`。
- scope 的 `room_id`、subject/grant 的 `principal_id` 以及 grant 的 Room 都必须已存在；不存在为 404。scope DELETE 也要求 body 指定的 Room 仍存在。
- DELETE scope/subject 的 body 值必须匹配当前记录；grant 的 `room_id/principal_id` 必须与路径一致。任何不匹配返回 409 `conflict`，不存在的绑定/链接/grant 返回 404 `not_found`。
- grant 当前唯一支持的 capability 是字面量 `"controller"`；其它值返回 400 `bad_request`。

## 7. 错误码

WS 错误仍使用 `{"type":"error","ref":"...","data":{"code","message"}}`；REST 使用 6.1 的 `{"error":{"code","message"}}`。`message` 面向诊断，可变；Client MUST 按 `code` 分支。

| code | REST 常用 HTTP | 含义 |
|---|---:|---|
| `unauthorized` | 401 | 未认证、session 无效/过期/吊销，或当前 Principal disabled/不存在 |
| `forbidden` | 403 | role、Room capability 或条目所有权不足；actor resolve 的 Principal 不可用 |
| `bad_request` | 400 | JSON/参数非法、严格请求出现未知字段、track_ref/source 格式错误 |
| `queue_full` | — | WS：达到房间 `max_queue` 上限；合同 A REST 当前返回 `conflict` |
| `quota_exceeded` | — | WS：超出 `policy.queue_limits`；合同 A REST 当前返回 `conflict` |
| `not_found` | 404 | Room、条目、媒体、Principal、Integration 或映射不存在 |
| `conflict` | 409 | REST 当前状态冲突、重复资源、请求与已有绑定不匹配 |
| `provider_error` | 502 | Provider 调用失败（附诊断 message） |
| `not_supported` | 501 | Provider 未实现可选能力（当前用于歌词） |
| `internal` | 500 | 服务端内部错误 |
| `rate_limited` | 429 | 同一来源 IP 在 10 分钟内已有 10 次非空错误管理员口令探测；窗口内后续带口令的 guest 认证被限制，无口令访客不受限 |

## 8. 明确不做的

- 投票切歌、DJ 模式（policy_json 可扩展，不实现逻辑）
- 播放进度持久化（重启后当前曲目丢失；队列保留在 DB，房间启动时自动续播队首）
- 队列事件的增量 diff 下发
- 账号密码登录（guest + 全局管理员口令 + OIDC 已覆盖当前认证需求）

## 9. 客户端实现清单

一个新 Client（官方 WebUI/CLI/Agent，或 AstrBot Plugin、Game Mod）先按 §0 选择协议面。所有展示和外部命令语法都留在 Client：

- 交互式 WebUI/CLI 通常用 REST 做目录/管理，用 WS 加房订阅实时状态并发命令。
- 收听 Agent 用 WS 快照驱动播放器；只有要接受管理员远程音量/换房时才实现 player plane。
- Integration Client 的最小流程是：解析外部事件 → 用稳定的 adapter/scope/subject 调 3.6 resolve → 取 `default_room_id` 或 Client 自己选择 Room → 用 `actor_token` 调 6.3 标准 REST → 把领域 JSON 渲染成聊天/游戏内容。需要实时订阅时，再用同一 `actor_token` 做 WS session 认证。
- Client 不得向不存在的 `/im/v1` 发请求，也不得期待 Server 理解诸如“点歌 xxx”的原始文本。

### 9.1 WS 连接时序

```
1. 连接 WebSocket /ws/v1
2. 校时：3–5 轮 ping/pong（§2.1），得 offset
3. auth：guest name/password，或 {"session_token": "..."}（OIDC/actor token 均可）
4. room.join（§4.2）→ 等 room.joined；即使用了 default_room_id，仍须提供该 Room 的访客密码
5. 接收四条状态快照（§4.1）：playback / queue / radio / listeners
6. 进入事件循环：四类 *.changed 广播覆盖本地副本
```

只做一次性命令/查询的 REST Client 无需建立 WS、校时或 join；需要实时订阅时才执行上述流程。

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

- WS 连接和 Room listener 关系是内存态：断线即离房。普通 session 持久化且可在过期/吊销前复用；Integration `actor_token` 只有效 5 分钟，过期后须重新 resolve。
- WS 重连后 MUST 重新校时、以仍有效的 session token 认证并 join 以取得全量快照；token 已失效则先重新登录/resolve。
- 服务端会主动断开发送缓冲满的慢客户端；客户端实现 SHOULD 指数退避重连（如 1s/2s/5s/10s 封顶）。

### 9.5 其他纪律

- MUST 忽略响应 JSON 中未知字段（版本纪律，§0）；不得据此给严格请求擅自增加字段。
- WS MUST 用 `ref` 匹配请求与响应（§4.3）；广播消息无 `ref`。REST 按 HTTP 请求本身匹配响应。
- SHOULD 假定任何操作都可能失败：WS 读取 `error.data.code`，REST 读取 `error.code`（§7），均按 code 而非 message 分支。
- 服务器位置推算的唯一依据是 playback 五元组 + offset（§2.2）；不要信任本地计时外推超过校偏间隔。

### 9.6 参考实现

仓库 `internal/client/` 是本协议 WS、无状态 Room REST 与 Integration actor/管理 API 的 Go 参考实现；`cmd/yuzu-agent/`（MPV 渲染代理）与 `cmd/yuzu-cli/`（控制端）是两类 Client 的最小完整示例。

只读查询对应的 Go REST helper 可直接供第一方 CLI/WebUI 复用：

```go
caps, err := client.RESTRoomCapabilities(ctx, server, sessionToken, roomID)

integrations, err := client.RESTListIntegrations(ctx, server, roomAdminToken)
scopes, err := client.RESTListIntegrationScopes(ctx, server, roomAdminToken, integrationID)
subjects, err := client.RESTListIntegrationSubjects(ctx, server, roomAdminToken, integrationID)
principals, err := client.RESTListPrincipals(ctx, server, roomAdminToken, query, limit)
grants, err := client.RESTListRoomGrants(ctx, server, roomAdminToken, roomID)
```

`caps.Controller` 是服务端 Authorizer 的最终判断；管理查询返回的 DTO 可用于选择目标后调用既有 `RESTBindIntegrationScope`、`RESTUnbindIntegrationScope`、`RESTLinkIntegrationSubject`、`RESTUnlinkIntegrationSubject`、`RESTGrantRoomController` 与 `RESTRevokeRoomController`，无需读取配置文件或服务端 secret。
