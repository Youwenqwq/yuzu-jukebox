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
| WebSocket | `/ws/v1` | 校时、认证、加入房间、实时状态广播，以及按 revision 分块的队列 snapshot/patch；也可发送与 REST 共用领域服务的房间命令 |
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

- **起播提前量（`should_be < 0`）**：切歌时服务端把新曲目的 position 0 排在
  「切歌时刻 + 提前量」，因此 `position_ms` MAY 为负，推算出的 `should_be`
  在这段窗口内同样为负，语义是「距本曲开播还有 `|should_be|` 毫秒」，预定
  开播的服务端时刻为 `updated_at + (-position_ms)`。提前量由房间策略
  `start_lead_ms` 控制（缺省 600ms，0 = 关闭）。
  - 存在这段窗口的原因：客户端从收到 `playback.changed` 到第一个采样出声要经过
    投递、HTTP 首字节、demux、音频设备灌注，累计数百毫秒。若房间时钟在切歌
    瞬间就从 0 起跑，客户端装载完成时只能 seek 到已经走过的位置——**曲目头部
    被确定性丢弃**。把 position 0 推到未来，这段延迟就被窗口吸收。
  - 客户端处理：`should_be < 0` 时 SHOULD 立即装载媒体并**保持暂停**（定位到 0）
    占住窗口预缓冲，到预定时刻再开声；MUST NOT 在此期间 seek（目标为负）或
    学习 drift 基线。窗口内 `playing` 已经是 `true`——它表示时间线在跑，
    **不能**用它判断是否该出声，只能看 `should_be` 的正负。
  - 提前量窗口内暂停/恢复走的是同一个纯函数：暂停冻结负 position，恢复从剩余
    倒计时续算，客户端无需特殊处理。
  - 渲染进度时 MUST 把负值钳到 0（含大厅目录的 `now_playing` 摘要，它是同一对
    `position_ms + updated_at` 字段）。
  - 中途进房/重连时 `should_be` 通常已 ≥ 0，此时按常规定位——那段头部在房间
    时间线上确实已经播过去了。
  - 客户端 MAY 用已学到的设备输出延迟基线把开声动作提前同等毫秒，让声音而非
    指令落在预定时刻上（见下方输出延迟区分）。
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
- **房间访问凭据不在此处传递**——它只作为受保护 Room 的 `room.join.password` 最终 fallback；身份具备免密准入条件时无需提供（见 4.2、6.2）。
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

可信 Integration 是数据库中的持久资源，由 `room_admin` 通过 6.4 的管理 API、WebUI 或 CLI 创建。创建与轮换时服务端生成高熵 token；**明文只在该次响应中返回一次**，数据库只保存 SHA-256 hash。

- Integration ID 必须匹配 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`；名称 trim 后必须为 1–100 个字符。
- Integration 有 `active` 状态。停用、token 轮换或删除会立即吊销该 Integration 已签发的全部 actor session；旧 token 下一次 resolve 也立即失败。
- 删除会级联清理该 Integration 的 scope、subject、actor session 与幂等记录，但不删除 Principal 或 Room grant。Principal/grant 可由管理员独立复用或清理。
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
- `default_room_id` 来自 external scope 绑定；未绑定时该字段**省略**，不是空值错误。它只是 Client 的默认选择，不自动加入 WS Room。该 actor session 加入这个 Room 时免交 guest credential；读取当前动态码仍可用于把码展示或分享给需要以 guest 身份加入的用户。
- `integration_id` 从通过认证的 Integration token 得出，请求体不能冒充。external scope key 为
  `(integration_id, adapter_id, scope.type, scope.id)`；external subject key 再追加 `subject.id`。`display_name` 不参与 key：未链接的 synthetic guest 在下次 resolve 时更新名称；已链接时忽略它并以当前 Principal 名称为准。
- 若 external subject key 已链接到一个 Principal，返回该 Principal 的**当前** identity/roles；若未链接，则生成稳定 guest：ID 为 `ig_` + `sha256(JSON([integration_id, adapter_id, scope.type, scope.id, subject.id]))` 的完整十六进制，`kind=guest`，roles 为 `listener+requester`。Client MUST 把返回的 `identity.id` 当作不透明值。
- scope 绑定、subject 链接和 controller grant 的管理合同见 6.4。
- actor session 持久记录签发它的 `integration_id/adapter_id/scope.type/scope.id`。服务端据此执行 scope-specific 授权；这些来源字段不能由 Client 在 Room 请求中覆盖。额外授权仅限：免 guest credential 加入 actor resolve 的 `default_room_id`，以及 4.8 中同时命中该 Room 与 policy 的 headless output 音量控制；两者都不能扩展到其它 Room。

### 3.7 OIDC 自助绑定 external subject

已通过普通 OIDC session 登录的用户可在任意 Client 中生成一次性绑定码，再从目标 IM subject 通过可信 Integration 消费该码。Server 只接收标准化的 external key，不解析平台消息文本。

```http
POST /api/v1/auth/external-binding-codes
Authorization: Bearer <oidc_session_token>
```

请求无 body；成功为 HTTP 201：

```json
{ "code": "7K3M-9P2D-X4RT", "expires_at": 1720000600000 }
```

- 必须是 Principal 含非空 `oidc_subject` 的普通登录 session；guest/password session 以及任何 `integration_id` 非空的 actor session 均为 403 `forbidden`。
- 绑定码为 12 位 Crockford Base32（展示时按 4 位分组），固定有效期 10 分钟。服务端只保存 SHA-256 hash；同一 Principal 新签发会使旧码失效，code/hash 不写入 audit。
- 绑定码不预绑定 Integration；持有有效 Integration token 的 Client 可从用户实际发送绑定命令的平台事件中消费。Integration Client MUST 使用事件自身的 adapter/scope/subject，MUST NOT 接受消息发送者指定这些标识。

兑换：

```http
POST /api/v1/integrations/bindings/redeem
Authorization: Bearer <integration_token>
Content-Type: application/json
```

```json
{
  "code": "7K3M-9P2D-X4RT",
  "adapter_id": "astrbot",
  "scope": { "type": "group", "id": "123456" },
  "subject": { "id": "9988" }
}
```

成功为 HTTP 200，返回写入的完整 external key、`principal_id` 及当前 OIDC identity。兑换只建立 `external_identity_links`，不签发 actor session；后续操作继续使用 3.6 的 actor resolve。

- 首次兑换原子消费绑定码并建立 subject→Principal 链接；相同 Integration、相同完整 external key 的网络重试返回相同成功结果，并携带 `Idempotency-Replayed: true`。
- 已消费的码不能用于其他 external key；错误、过期或已被其他 key 消费的码返回 400 `invalid_binding_code`。
- external key 已链接到同一 Principal 时视为成功；已链接到其他 Principal 时返回 409 `conflict`，自助流程绝不覆盖。`room_admin` 可继续使用 6.4 的管理接口处理迁移或恢复。
- 绑定沿用 3.6 的 scope-specific key；同一平台账号出现在不同 scope 时需要分别绑定。绑定只影响后续 actor resolve，不改写 synthetic guest 的既有队列、历史或 audit 归属。

### 3.8 持久 Player 与 Player key

Headless Agent 不是 guest/OIDC Principal，也不使用管理员口令。`room_admin` 先创建持久 `Player` 资源；服务端只在创建或轮换时返回一次明文 Player key，数据库仅保存 SHA-256 hash。

```json
→ { "type": "auth", "ref": "1", "data": { "player_key": "yzp_…" } }
← { "type": "auth.ok", "ref": "1", "data": {
      "identity": { "id": "pl_…", "name": "Living Room", "kind": "player" }
  } }

→ { "type": "player.hello", "ref": "2", "data": {
      "device": "living-room-host", "version": "1.0.0",
      "caps": ["volume", "mute"]
  } }
← { "type": "player.hello.ok", "ref": "2", "data": { "player_id": "living-room" } }
```

- `player_key` 不能与 `name/password/session_token` 混用。认证后连接只允许 `ping`、`player.hello`、`player.state`；Player 没有 Yuzu role，不能作为 Principal 点歌或控制 Room。
- `player.hello` 不接受客户端声明的 Player ID；服务端只信任 key 解析出的持久 Player。每个 Player ID 同时只保留一条在线连接，新连接会关闭旧连接。
- Room 分配由 `room_admin` 持久管理。若 Player 已分配 Room，`player.hello.ok` 之后服务端自动推送 `room.joined` 与 `playback.changed`；未分配时 Agent 保持在线等待。Player 不能自行 `room.join`/`room.leave`。Headless Player 是输出端，不订阅 `queue.snapshot`、`queue.patch`、`radio.changed` 或 `listeners.changed`，也不进入 `listeners` 列表；其加入/离开不触发 `listeners.changed`，也不计入大厅 `listener_count`。
- 停用 Player、轮换 key 或删除 Player 都会立即关闭在线连接。停用保留 key 与 Room 分配；重新启用后可用原 key 重连。轮换后旧 key 立即失效；删除级联清理 Room 分配。
- `last_seen_at` 在成功 Player-key 认证时更新。Agent SHOULD 对断线使用有上限的指数退避；连续稳定运行后重置退避。
- 从旧版自声明 `player_id` 升级时，已有 Room 分配迁移为 `active=false`、尚无 key 的 Player；管理员必须先轮换 key，再显式启用。没有凭据的 Player 不能启用。

## 4. 房间会话

### 4.1 入房状态与 revision 队列协议

`room.join` 成功后，服务端先返回带原请求 `ref` 的：

```json
{ "type": "room.joined", "ref": "join-1", "data": { "room_id": "lobby" } }
```

随后发布当前 `playback.changed`、一个逻辑 `queue.snapshot`、`radio.changed` 与 `listeners.changed`。客户端 MUST 按 `type` 分派，不能依赖这些状态消息之间的固定条数或固定相对顺序：队列快照可拆成任意数量的 part，入房期间也可能发生新的状态广播。唯一需要按序组装的是同一个逻辑队列 snapshot/patch 的 part。

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

**队列基线 `queue.snapshot`：**

```json
{ "type": "queue.snapshot", "data": {
  "revision": 42,
  "part": 0,
  "items": [ /* 零个或多个队列条目 */ ],
  "done": false
} }
{ "type": "queue.snapshot", "data": {
  "revision": 42,
  "part": 1,
  "items": [ /* 余下条目 */ ],
  "done": true
} }
```

- `revision` 是该 Room 待播队列的版本。一个逻辑快照的所有 part 使用同一 revision；每个逻辑快照都从 `part: 0` 开始，part 连续递增，且只有最后一个 part 为 `done: true`。空队列仍发送一条 `part:0/items:[]/done:true`。
- 客户端 MUST 先在临时缓冲区拼接全部 `items`，收到 `done:true` 后才把本地队列和 revision **同时原子替换**。不得逐 part 更新 UI，也不得把不同 revision 的 part 混合。
- 队列条目字段同 `current`，但不含 `stream_url`、`size_bytes` 或 `bitrate_kbps`。

**增量 `queue.patch`：**

```json
{ "type": "queue.patch", "data": {
  "base_revision": 42,
  "revision": 43,
  "part": 0,
  "ops": [
    {"op":"add", "index":2, "item":{ /* 完整队列条目 */ }},
    {"op":"move", "entry_id":"e_991", "to_index":0},
    {"op":"remove", "entry_id":"e_123"}
  ],
  "done": true
} }
```

- 一次逻辑队列变更恰好把 revision 从 `base_revision` 推进到 `revision = base_revision + 1`。若 ops 过大，它可拆为多个 part；所有 part 的两个 revision 相同，`part` 从 0 连续递增，最后一 part 才 `done:true`。
- `add` 在当时队列的零基 `index` 插入完整 `item`；`remove` 按 `entry_id` 删除；`move` 把该 `entry_id` 移到当时队列的零基 `to_index`；`clear` 无其它字段并把待播队列置空。多个 op MUST 按数组顺序作用。
- 客户端 MUST 先收齐所有 part，再在本地队列副本上依序验证并执行全部 ops；全部成功后才同时提交新队列与新 revision。任一 op 非法时不得部分提交。

每个 `queue.snapshot` 或 `queue.patch` 的**完整 JSON 信封**（包含 `type`、`data`、字段名和转义开销）不超过 **24 KiB**。分块边界按实际 JSON 编码字节数决定，不按条目数决定；该预算低于 coder/websocket 保持的 **32 KiB ReadLimit**。客户端 MUST 接受任意合法分块，不能假设每 part 的 items/ops 数量。单个队列条目仍必须能装入一个 envelope。

**丢包、乱序与 resync：**

客户端遇到以下任一情况 MUST 保留最后一次已提交队列、丢弃当前临时组装，并发送 `queue.sync`：part 不从 0 开始或不连续、同一组内 revision 改变、patch 的 `base_revision` 不等于本地 revision、`revision != base_revision + 1`，或 op 无法合法应用。

```json
→ { "type": "queue.sync", "ref": "sync-7", "data": { "room_id": "lobby" } }
← { "type": "queue.snapshot", "data": {
     "revision": 57, "part": 0, "items": [], "done": true
   } }
← { "type": "ack", "ref": "sync-7", "data": {} }
```

`queue.sync` 只允许已认证且已加入同一 Room、并订阅队列的连接；服务端返回一个新的分块 snapshot 及带 ref 的 ack，snapshot 可先于 ack 到达。新的 snapshot 完成前客户端不得用不连续 patch 改写展示；完成后以其 revision 为新基线继续处理后续 patch。慢客户端被断开时，重连并重新 join 同样会获得新基线。

**曲目元数据层次**（客户端只需面对这一个形状，字段可空即降级）：

- 曲目层（入队 snapshot/patch 与播放广播都带）：`title/artist/duration_ms/album/cover_url/source_url/contributors/requester_name`。`requester_name` 是入队时操作者 identity 的显示名快照，请求人后来改名不影响历史条目；旧数据为空串时客户端可降级显示 `requested_by`。`cover_url` 一律为服务端代理路径 `/api/v1/cover/{track_ref}`（源站可能需 Referer）。
- 物理层（仅 `playback.current`，Resolve/缓存后可得）：`size_bytes/bitrate_kbps`。
- provider 能力缺席合法：bili 无歌词、local 无 source_url，字段缺省即降级。

**`listeners.changed` 的 data：**`{"listeners": [ {"id": "...", "name": "..."}, ... ]}`。仅含普通 Client / Integration 的 Room 会话；Headless Player 不算 listener。

**`radio.changed` 的 data：**`{"radio": null | {"source": "...", "description": "...", "finite": true, "shuffle": false, "once": false}}`（见 4.5）

### 4.2 客户端 → 服务端消息

| type | data | 权限 | 说明 |
|---|---|---|---|
| `room.join` | `{room_id, password?}` | 任何已认证身份 | `password` 是受保护 Room 的 guest credential fallback；免密准入见下文 |
| `room.leave` | `{room_id}` | — | |
| `queue.sync` | `{room_id}` | 已加入同一 Room 的队列订阅者 | 请求新的 `queue.snapshot` 基线；revision/part 异常时使用 |
| `queue.add` | `{room_id, track_ref}` 或 `{room_id, track_refs[1..100]}` | `requester` | 队尾追加；批量为原子：整体预校验，任一失败一条不加；ack 回 entry_ids |
| `queue.remove` | `{room_id, entry_id}` | 条目所有者 / 该 Room 的 controller | 只作用于待播队列 |
| `queue.move` | `{room_id, entry_id, to_index}` | 该 Room 的 controller | |
| `queue.clear` | `{room_id}` | 该 Room 的 controller | 原子清空待播队列；保留 `playback.current` 与 radio 绑定；空队列也成功 |
| `playback.pause` / `playback.resume` | `{room_id}` | 该 Room 的 controller | |
| `playback.seek` | `{room_id, position_ms}` | 该 Room 的 controller | |
| `playback.skip` | `{room_id}` | 该 Room 的 controller | 切下一首 |
| `radio.play` | `{room_id, source, shuffle, once}` | 该 Room 的 controller | 进入电台模式（见 4.5） |
| `radio.stop` | `{room_id}` | 该 Room 的 controller | 退出电台模式 |

受保护 Room 的入房判定按以下顺序执行：

1. `guest_access.mode == "open"`：任何已认证身份免凭据；
2. 当前身份有 `room_admin`，或拥有该**准确 Room** 的显式 `controller` grant：免凭据；
3. 当前 session 是由映射到该**同一 Room** 的 Integration scope 签发的 actor session：免凭据，不能跨 Room；
4. 当前身份 roles 命中 Room 的任一 `trusted_roles`：免凭据；
5. 只有都未命中时，才把 `room.join.password` 当作 static password 或 rotating code 校验。

因此 guest access credential 是 fallback，不是所有身份的前置条件。普通 guest 通常走第 5 步；动态码继续用于向这些 guest 分享入房权限。只有实际进入第 5 步的错误凭据探测才计入 Room 凭据限流。

### 4.3 服务端 → 客户端消息

| type | 时机 | data 要点 |
|---|---|---|
| `ack` | 请求处理成功 | 空对象；**回带 `ref`**；`queue.add` 时回 `{"entry_ids": [...]}` |
| `error` | 请求失败 | `{code, message}`；**回带 `ref`** |
| `room.joined` | `room.join` 成功 | `{"room_id"}`；回带 `ref` |
| `room.left` | `room.leave` 成功 | 空对象；回带 `ref` |
| `pong` | 响应 `ping` | `{client_time, server_time}`；回带 `ref` |
| `playback.changed` | 播放/暂停/seek/切歌/自然结束/新听众进房 | 完整 `playback` 对象（见 4.1） |
| `queue.snapshot` | 入房或 `queue.sync` | revisioned 完整基线；按 `part/done` 分块 |
| `queue.patch` | 待播队列逻辑变更 | 从 `base_revision` 到 `revision` 的原子 ops；按 `part/done` 分块 |
| `listeners.changed` | 听众进出 | `{"listeners": [...]}` |
| `radio.changed` | 电台开启/停止/新听众进房 | `{"radio": null | {...}}` |

**请求-响应匹配**：请求的 `ack`/`error` 回带触发请求的 `ref`；状态广播与 `queue.snapshot`/`queue.patch` 不带 `ref`。客户端 MUST 用 `ref` 路由响应，并独立按 `type` 处理队列组装。player plane 的异步 `player.command` 是独立的无 `ref` 服务端推送（见 4.8）。

**关键设计：`playback.changed` 永远携带完整 playback 对象。** 客户端无需区分事件原因，统一按"以最新状态重算 should_be"处理。新增听众也凭此一次性对齐。

### 4.4 WS 与无状态 REST 的操作边界

- WS 房间命令要求连接先以 `room.join` 加入同一个 Room；只有需要走 guest fallback 时才按 Room 当前访问模式解释 `room.join.password`。成功加入会建立实时广播订阅。
- 6.3 的 Room REST 不建立 listener、不要求先 join，也不接收或检查 guest credential、`trusted_roles` 或其它 WS admission 条件；它只校验标准 Bearer session 及各操作的 requester/controller 权限。
- 两种传输适配器调用同一个 Room 领域服务和同一个 actor。REST 命令造成的状态变化会广播为 `queue.patch` 或对应状态消息给已加入该 Room 的 WS 客户端；WS 命令也可由后续 REST state 查询观察。
- `GET .../state` 是一次性、按调用身份投影的完整快照，不是订阅。需要实时变化时仍 MUST 使用 `/ws/v1` 加房，并处理 `playback.changed`、`queue.snapshot`/`queue.patch`、`radio.changed` 与 `listeners.changed`。

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
{
  "max_queue": 100,
  "queue_limits": { "guest": 5, "room_admin": 0 },
  "member_player_volume": true,
  "start_lead_ms": 600
}
```

- `max_queue`：待播队列总上限，0/缺省 = 不限。超限拒绝 `queue.add`，错误码 `queue_full`。
- `queue_limits`：按身份的待播数上限。key 匹配身份的 **kind**（`guest`/`password`/`oidc`）或其任一 **role**；任一命中值为 0 → 显式不限（覆盖其他命中），否则取命中最大值，无命中 = 不限。超限拒绝，错误码 `quota_exceeded`。
- `member_player_volume`：缺省 `false`。为 `true` 时，只允许由可信 Integration 签发、且来源 scope 当前默认 Room 正是该 Room 的 actor session 读取和设置 Room headless output desired volume；不授权普通 listener，不授权其他 Room，不授权单设备 mute、换房或绑定管理。
- `start_lead_ms`：切歌起播提前量（毫秒），取值 `[0, 5000]`，缺省 600，0 = 关闭（切歌即 position 0）。房间内客户端装载普遍慢（公网带宽小、蓝牙输出）时调大；纯本地局域网可调小。语义与客户端处理见 §2.2。
- 普通 guest 身份 ID 由名字确定性派生（`g_` + `sha256("guest:"+name)` 前 12 hex），同名重连仍是同一人；Integration synthetic guest 使用 3.6 的完整 external subject key——两者的限额与“移除自己点的歌”均可跨会话成立。
- 电台补充不经过限额检查（补充只在队列 <3 时触发，天然有界）。
- 播放历史与统计见 6 节 REST；`play_history` 只记**播放**（结束原因 `finished`/`skipped`），不记点歌意图。

### 4.8 播放端管理平面（player plane）

无头渲染端（嵌入式播放器）使用 3.8 的 Player key 注册。一个 Room 可包含多个持久 Player；Room output 保存一个权威 desired volume，并向当前 Room 的在线 Agent fan-out：

```json
agent:  → {"type":"auth","data":{"player_key":"yzp_…"}}
server: ← {"type":"auth.ok","data":{"identity":{"kind":"player",…}}}
agent:  → {"type":"player.hello","data":{"device":"speaker-01","version":"1.0.0","caps":["volume","mute"]}}
server: ← {"type":"player.hello.ok","data":{"player_id":"living-room-left"}}
agent:  → {"type":"player.state","data":{"volume":42,"muted":false}}
server: ← {"type":"player.command","data":{"op":"set_volume","value":37}}
```

- Player ID 只由 key 的持久记录决定；`player.hello` 不能声明 ID。在线连接断开只清除 runtime state，不删除 Player 或 Room 分配。
- 一个 Room 可持久分配多个 Player，一个 Player 最多属于一个 Room。`room_admin` 用 `PUT /api/v1/rooms/{id}/players/{player_id}` 分配在线或离线 Player；在线时立即迁入。对应 DELETE 解除分配并使在线 Agent 立即离房但保持连接；`GET .../players` 始终包含离线分配。
- Room output volume 独立持久化。未设置时 `GET /api/v1/rooms/{id}/output` 返回 `"volume":null`，服务端不会覆盖设备本地音量；首次 PATCH 后成为 Room desired state。
- `PATCH /api/v1/rooms/{id}/output {"volume":0..100}` 先持久化 desired volume，再向当前 Room 内所有已 `player.hello` 且声明 `volume` capability 的连接下发 `player.command`。没有在线 Agent 仍成功；离线 Agent、重连 Agent 或之后迁入该 Room 的 Agent 会在注册/加入时自动收敛。
- `player.state.volume` 只表示单设备实际状态，不反向修改 Room desired volume。管理员的单设备命令属于临时偏离，下一次 Room output 更新或 Agent 重连会覆盖它。
- `player.command` 仅有 `set_volume` 与 `set_mute`。换房不是设备命令：Player 没有 Room 密码，也不能自行加入或离开；持久分配是唯一权威。
- Room controller 可读写 output。普通 Integration actor 仅在 `policy.member_player_volume=true` 且 actor session 的签发 scope 当前映射到该 Room 时可读写；写请求必须带平台事件的 `Idempotency-Key`。Integration actor 不能管理 Player 资源、Room 分配或指定 `player_id`。
- 普通 WebUI 不持有 Player key、不进入 player registry，也不会收到 `player.command`；浏览器本地音量完全不受 Room output 影响。

## 5. 房间状态机

### 5.1 状态字段（权威，存于房间 actor 内）

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

`Current` 与 `Queue` 是 actor 的内存表示：`Queue` 只含待播条目，`Current` 单独经
`playback.changed` 下发。持久层的切分点不同——`room_queue` 同时保存当前曲目与待播条目，
由 `is_current` 标记游标。两者的差异只存在于存储层，线上格式不受影响：
`queue.snapshot` / `queue.patch` 携带的始终是待播条目。

当前曲目落库有两个后果。其一，正在流式传输的曲目对 SQL 可见，加速层可以按
「游标起的前 N 条」导出预取视界并钉住这些对象，而不必去问房间 actor 要运行时状态。
其二，重启从当前曲目本身续播，而不是把它当作已出队、跳到下一首。`PositionMs` 仍是
运行时状态，所以续播从曲目开头开始。

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
- 广播由 actor 按状态类型组装一次，扇出给各 listener 的发送队列；队列消息按 4.1 的 envelope 预算分块。慢客户端（发送缓冲满）直接断开，由客户端重连 + 重新 join 取得新的 revisioned 队列基线与其它当前状态。

## 6. REST API（`/api/v1`）

### 6.1 共通合同

- 端点权限以 6.2 为准。受保护的标准 REST 使用 `Authorization: Bearer <session_token>`，token 可来自 guest/OIDC 登录或 3.6 的 `actor_token`；Integration token 不是标准 session。guest/OIDC/config 与 cover 是公开端点，actor resolve 改用 Integration token，logout 只要求非空 Bearer 值，stream 改用查询参数 ticket。
- Server 返回领域 JSON，不返回已经排版的聊天文本。Integration Client 自行把结果转换成 AstrBot 消息链、游戏 UI、终端文本等。
- 客户端 MUST 忽略**响应**中的未知字段。6.3 中带 JSON body 的 Room 控制端点与 6.4 的 Integration 端点采用严格 JSON 解码：未知字段、类型不符、多个 JSON 值均返回 HTTP 400 `bad_request`；Room 控制 JSON 解码器最多读取 1 MiB。
- 6.3 的六个 Room 写端点支持 `Idempotency-Key`：`POST queue`、`DELETE queue`、`DELETE queue/{entry_id}`、`POST playback`、`POST radio`、`DELETE radio`。Integration actor session 调用这些端点时此 header **必填**；普通 guest/OIDC session 可选。key 最长 200 bytes，应使用外部平台 event/message ID 派生的稳定值。
- 去重作用域是 `(actor_id, integration_id, key, HTTP method, escaped path)`。同一作用域和逐字节相同的 request body 在 24 小时内重放缓存的非 5xx status/body，并返回 `Idempotency-Replayed: true`；同 key 不同 body 返回 409 `idempotency_conflict`；首个请求仍处理中返回 409 `request_in_progress`。5xx 不缓存，可安全重试。`DELETE .../queue` 即使不用同一 key 重复调用也保持语义幂等：待播队列仍为空且请求成功。
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
| `POST /api/v1/auth/external-binding-codes` | 普通 OIDC session | 签发 10 分钟一次性 external subject 绑定码；HTTP 201；不接受 actor session |
| `GET /api/v1/integrations` | `room_admin` | 持久 Integration 元数据列表；绝不返回 token/hash |
| `POST /api/v1/integrations` | `room_admin` | 创建 Integration，HTTP 201；明文 token 只在本次响应返回 |
| `PATCH /api/v1/integrations/{id}` | `room_admin` | 修改名称和/或 active；停用立即吊销 actor sessions |
| `POST /api/v1/integrations/{id}/token` | `room_admin` | 轮换 token；旧 token 与 actor sessions 立即失效，新 token 只返回一次 |
| `DELETE /api/v1/integrations/{id}` | `room_admin` | 删除 Integration 及其 scope/subject/session/idempotency 数据 |
| `POST /api/v1/integrations/actors/resolve` | Integration token | external scope/subject → 5 分钟标准 actor session（见 3.6、6.4） |
| `POST /api/v1/integrations/bindings/redeem` | Integration token | 从真实 external scope/subject 消费绑定码并建立 Principal 链接（见 3.7、6.4） |
| `GET /api/v1/integrations/{id}/scopes` | `room_admin` | 该 Integration 的全部 external scope→Room 绑定，稳定排序 |
| `PUT/DELETE /api/v1/integrations/{id}/scopes` | `room_admin` | 绑定/解绑 external scope 的默认 Room |
| `GET /api/v1/integrations/{id}/subjects` | `room_admin` | 该 Integration 的全部 external subject→Principal 链接，稳定排序 |
| `PUT/DELETE /api/v1/integrations/{id}/subjects` | `room_admin` | 链接/解绑 external subject 与 Principal |
| `GET /api/v1/principals?q=&limit=` | `room_admin` | 按 ID 或名称可选搜索 Principal；默认/上限 100，稳定排序；不返回 OIDC subject |
| `GET /api/v1/rooms` / `GET /api/v1/rooms/{id}` | `listener` | 房间目录/单房间信息；字段合同见下文；绝不含静态密码、当前动态码或 `stream_url` |
| `POST /api/v1/rooms` / `PATCH /api/v1/rooms/{id}` | `room_admin` | 建/改房间名称、访问模式、`trusted_roles` 及 policy；访问配置热更新 |
| `GET /api/v1/rooms/{id}/access-code` | `room_admin`，或当前 scope 映射到该 Room 的 Integration actor | 返回当前动态验证码与有效期，供分享给走 guest fallback 的用户；`Cache-Control: no-store` |
| `DELETE /api/v1/rooms/{id}` | `room_admin` | 删除房间（队列与历史级联清理） |
| `GET /api/v1/rooms/{id}/grants` | `room_admin` | 该 Room 的全部显式 `controller` grants，稳定排序 |
| `PUT/DELETE /api/v1/rooms/{id}/grants/{principal_id}` | `room_admin` | 授予/撤销该 Principal 的 Room `controller` capability |
| `GET /api/v1/rooms/{id}/history?offset=&limit=` | `listener` | 播放历史，最新在前（默认 50，上限 200） |
| `GET /api/v1/rooms/{id}/stats?limit=` | `listener` | 曲目热度榜（默认 20，上限 100） |
| `GET /api/v1/rooms/{id}/capabilities` | 标准 session | 当前身份的有效 Room capability；Room 不存在为 404 |
| `GET /api/v1/rooms/{id}/state` | 标准 session | 无副作用完整状态快照 |
| `POST /api/v1/rooms/{id}/queue` | `requester` | 单条或 1–100 条原子入队 |
| `DELETE /api/v1/rooms/{id}/queue` | controller | 原子清空待播队列；保留 current 与 radio；空队列也成功 |
| `DELETE /api/v1/rooms/{id}/queue/{entry_id}` | 所有者 / controller | 移除待播条目 |
| `PATCH /api/v1/rooms/{id}/queue/{entry_id}` | controller | 移动待播条目 |
| `POST /api/v1/rooms/{id}/playback/{op}` | controller | `pause/resume/skip/seek` |
| `POST /api/v1/rooms/{id}/radio` / `DELETE /api/v1/rooms/{id}/radio` | controller | 开启/停止电台 |
| `GET /api/v1/players` | `room_admin` | 全部持久 Player 与在线状态；不返回 key/hash |
| `POST /api/v1/players` | `room_admin` | 创建 `{"id","name"}`；HTTP 201，明文 key 只在本次响应返回 |
| `GET /api/v1/players/{id}` | `room_admin` | Player 元数据、Room 分配与当前在线状态 |
| `PATCH /api/v1/players/{id}` | `room_admin` | 修改 `name` 和/或 `active`；停用立即断开 Agent |
| `POST /api/v1/players/{id}/key` | `room_admin` | 轮换 key；旧 key 和在线连接立即失效，新 key 只返回一次 |
| `DELETE /api/v1/players/{id}` | `room_admin` | 删除 Player、Room 分配并断开在线 Agent |
| `POST /api/v1/players/{id}/command` | `room_admin` | 在线设备维护指令：`set_volume` / `set_mute` |
| `GET /api/v1/rooms/{id}/output` | controller，或同 Room 且 policy 允许的 Integration actor | Room headless output desired state；未设置时 `volume:null` |
| `PATCH /api/v1/rooms/{id}/output` | controller，或同 Room 且 policy 允许的 Integration actor | 持久化 `{"volume":0..100}` 并 fan-out；Integration actor 必须有 `Idempotency-Key` |
| `GET /api/v1/rooms/{id}/players` | `room_admin` | Room 持久分配与当前在线状态；离线 Player 仍在列表 |
| `PUT/DELETE /api/v1/rooms/{id}/players/{player_id}` | `room_admin` | 分配/解除持久 Player；允许离线分配，在线时立即进入/离开 Room |

**Room 管理 JSON 与访问配置：**

rotating-code Room 的创建请求示例：

```json
{
  "id": "lobby",
  "name": "大厅",
  "guest_access_mode": "rotating_code",
  "guest_code_period_seconds": 86400,
  "trusted_roles": ["vip", "staff"],
  "policy": "{\"max_queue\":500}"
}
```

- create 的 `name` 必填；`id` 可省略，服务端从 name 生成 slug。PATCH 可携带 `name`、`guest_access_mode`、`guest_password`、`guest_code_period_seconds`、`trusted_roles`、`policy` 中的任意子集，所有省略字段保持不变。`policy` 在 create/PATCH **请求**中是包含 JSON 对象文本的字符串；省略时创建为 `{}`。
- `guest_access_mode` 取 `open | static_password | rotating_code`。创建时省略该字段：非空 `guest_password` 推断为 `static_password`，否则为 `open`。
- `static_password` 创建或更换密码时必须携带非空、只写的 `guest_password`；服务端只保存 bcrypt hash，任何响应都不返回密码。显式 `open`/`rotating_code` 不接受非空 `guest_password`。PATCH 省略字段即保留；只携带 `guest_password` 时，非空切为静态密码、空串切为开放。
- `rotating_code` 的 `guest_code_period_seconds` 默认 `86400`（24 小时），范围为 `60..2592000`（1 分钟至 30 天）。动态码使用与 external binding code 相同的 12 字符无歧义 Base32 alphabet，展示为 `XXXX-XXXX-XXXX`；输入忽略大小写、连字符与 Unicode 空白。它由服务端 `secret_key`、Room ID/创建时间、周期和 UTC 时间 counter 经域隔离 HMAC-SHA256 派生，不落库、不需要轮换任务。
- `trusted_roles` 是免 guest credential 的身份 role 数组；省略时创建为空数组、PATCH 时保留，显式 `[]` 清空。每项 trim 后必须为 1–64 字节的 role 名：首字符为 ASCII 字母或数字，后续还可含 `_`、`-`、`.`。服务端去重并稳定排序。`listener` 与 `requester` **禁止**出现，因为它们会让普通 guest 普遍绕过房间保护；其它命中当前身份的 role 均按 4.2 免密。
- 周期切换后的前 15 秒接受上一周期码；之后只接受当前码。访问配置热更新只影响后续 join，不踢出已经加入的连接。错误 guest credential 按 `(room_id, source_ip)` 限制为 10 分钟窗口内 10 次；身份免密路径不计入探测次数，正确凭据清空失败桶，超过阈值返回 `rate_limited`。

`POST /api/v1/rooms` 成功返回 HTTP 201；`PATCH /api/v1/rooms/{id}` 成功返回 HTTP 200。两者响应形状相同，且 `code_period_seconds` 仅在 rotating mode 出现：

```json
{
  "room": {
    "id": "lobby",
    "name": "大厅",
    "guest_access": {
      "mode": "rotating_code",
      "code_period_seconds": 86400,
      "trusted_roles": ["staff", "vip"]
    }
  }
}
```

`GET /api/v1/rooms` 返回 `{"rooms":[room_info, ...]}`；`GET /api/v1/rooms/{id}` 返回 `{"room":room_info}`。`room_info` 的完整顶层字段如下：

```json
{
  "id": "lobby",
  "name": "大厅",
  "policy": {"max_queue": 500},
  "guest_access": {
    "mode": "rotating_code",
    "code_period_seconds": 86400,
    "trusted_roles": ["staff", "vip"]
  },
  "listener_count": 12,
  "now_playing": {
    "title": "海阔天空",
    "artist": "Beyond",
    "duration_ms": 326000,
    "cover_url": "/api/v1/cover/ncm%3A347230",
    "position_ms": 30210,
    "updated_at": 1720000000123,
    "playing": true,
    "rate": 1
  }
}
```

`policy` 在查询**响应**中是 JSON 对象，不是字符串。`now_playing` 空闲时为 `null`；它和 `guest_access` 都不泄露 `stream_url`、密码 hash、静态密码或当前动态码。`guest_access.trusted_roles` 始终是数组；`code_period_seconds` 只在 rotating mode 出现。

Room create/PATCH/list/get 的错误合同：

| HTTP / `error.code` | 条件 |
|---|---|
| 400 / `bad_request` | create 缺 name、JSON/访问模式/周期/password/trusted role/policy 非法 |
| 401 / `unauthorized` | session 缺失、过期、吊销，或 Principal 不可用 |
| 403 / `forbidden` | list/get 缺 `listener`，或 create/PATCH 缺 `room_admin` |
| 404 / `not_found` | get/PATCH 的 Room 不存在 |
| 409 / `conflict` | create 的 Room ID 已存在或其它持久化冲突 |
| 500 / `internal` | 目录快照、热更新或存储发生未分类服务端错误 |

`GET /api/v1/rooms/{id}/access-code` 响应：

```json
{
  "room_id": "lobby",
  "access_code": {
    "code": "7M2K-Q9TR-W4HX",
    "period_seconds": 86400,
    "valid_from": 1720000000000,
    "expires_at": 1720086400000
  }
}
```

普通 session 只有当前 `room_admin` 可读取。Integration actor 只有在其签发 scope **当前**映射到路径 Room 时可读取；Integration 停用、token 轮换、scope 解绑/改绑或 actor session 过期都会立即阻断。该端点用于把动态码安全地展示或分享给需要走 guest fallback 的用户；same-room Integration actor 自己入房不需要提交该码。响应和 audit detail 均不记录其它秘密，验证码正文不进入 audit。

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

这些路径都不建立 WS 连接或 Room listener，不要求 `room.join`，也不接收或检查 guest credential；WS admission 的 same-room Integration、`trusted_roles` 等免密判定在这里同样不参与授权。REST 保持无状态，只认证标准 Bearer session，再按操作检查 requester/controller。`controller` 的定义是全局 `room_admin` **OR** 此 Principal 在此 Room 的 `controller` grant（3.4）。

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

- 四个字段的领域形状与 4.1 相同；REST 顶层 `queue` 是一次性完整数组，不携带 WS 的 revision/part envelope，`radio` 也直接是值。
- `playback.current` 非空时包含按本次调用 identity 签发的 `stream_url`；队列条目仍不含 `stream_url`。
- `listeners` 只反映已加入的普通 WS listener（不含 Headless Player）。查询本身无副作用，重复查询不会把调用者加入列表。

**命令请求与成功响应：**

| 方法与路径 | JSON body | 权限 | HTTP 200 body |
|---|---|---|---|
| `POST /api/v1/rooms/{id}/queue` | `{"track_ref":"ncm:..."}` 或 `{"track_refs":["...", ...]}`，严格二选一；数组 1–100 | `requester` | `{"entry_ids":["e_...", ...]}`，顺序与请求一致 |
| `DELETE /api/v1/rooms/{id}/queue` | 无 | controller | `{}` |
| `DELETE /api/v1/rooms/{id}/queue/{entry_id}` | 无 | 条目所有者 / controller | `{}` |
| `PATCH /api/v1/rooms/{id}/queue/{entry_id}` | `{"to_index":0}`；零基、非负且须落在当前待播队列范围 | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/pause` | 无（空对象也可） | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/resume` | 无（空对象也可） | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/skip` | 无（空对象也可） | controller | `{}` |
| `POST /api/v1/rooms/{id}/playback/seek` | `{"position_ms":1234}`；非负整数，超过时长时 clamp | controller | `{}` |
| `POST /api/v1/rooms/{id}/radio` | `{"source":"playlist:...","shuffle":false,"once":false}` | controller | `{}` |
| `DELETE /api/v1/rooms/{id}/radio` | 无 | controller | `{}` |

- 单条与批量入队都会先解析全部 Track，再由 Room actor 整批预校验并追加；任一 ref、Provider、总量或身份限额失败时一条不加。
- clear 只清空 pending queue；正在播放的 `playback.current` 与 radio 绑定保持不变。空队列 clear 仍返回成功，因此调用在状态语义上幂等；WS 订阅者以收到的 revisioned `queue.patch` 为准。
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

### 6.4 Integration 生命周期、映射与 Room grant 管理

actor resolve 与绑定码兑换使用 Integration token；绑定码签发使用普通 OIDC session；本节其余管理端点必须使用具有当前 `room_admin` role 的**标准 session**。Integration token 本身调用管理端点会是 401，普通 actor token 默认只有 listener/requester，调用会是 403。

**OIDC 自助绑定：**

- `POST /api/v1/auth/external-binding-codes` 与 `POST /api/v1/integrations/bindings/redeem` 的严格合同见 3.7。
- 签发成功为 HTTP 201；无普通 OIDC session 为 401/403。兑换请求未知字段、缺失或空白字段为 400 `bad_request`。
- 兑换会在同一事务中重新确认 Integration active、token hash 未轮换、绑定码状态、OIDC Principal active 状态及当前 subject 链接；token 在请求期间失效为 401。
- 成功签发与首次成功绑定均写 audit，但 code/hash 永不进入 detail；精确重试不重复写绑定 audit。

**Resolve：**

- `POST /api/v1/integrations/actors/resolve` 的严格请求/响应形状见 3.6；成功为 HTTP 200。
- Integration token 缺失/错误为 401 `unauthorized`；字段缺失、空白、类型错误或未知字段为 400 `bad_request`；链接/guest Principal disabled 为 403 `forbidden`。

**管理查询：**

| 方法与路径 | HTTP 200 JSON 示例 |
|---|---|
| `GET /api/v1/integrations` | `{"integrations":[{"id":"generic-bridge","name":"Generic bridge","active":true,"created_at":1720000000000,"updated_at":1720000000000,"last_used_at":1720000010000}]}` |
| `GET /api/v1/integrations/{id}/scopes` | `{"scopes":[{"integration_id":"generic-bridge","adapter_id":"onebot","scope_type":"group","scope_id":"42","room_id":"main"}]}` |
| `GET /api/v1/integrations/{id}/subjects` | `{"subjects":[{"integration_id":"generic-bridge","adapter_id":"onebot","scope_type":"group","scope_id":"42","subject_id":"7","principal_id":"p_123"}]}` |
| `GET /api/v1/principals?q=&limit=` | `{"principals":[{"id":"p_123","name":"Alice","kind":"oidc","roles":["listener"],"active":true}]}` |
| `GET /api/v1/rooms/{id}/grants` | `{"grants":[{"room_id":"main","principal_id":"p_123","capability":"controller"}]}` |

所有列表均确定排序且空结果返回 `[]`。Integration 列表绝不含 token/hash；`last_used_at` 从未 resolve 时省略。Principal 列表只公开 `id/name/kind/roles/active`，绝不含 `oidc_subject`。`q` 可省略，按 ID 或名称匹配；`limit` 默认 100 且最大 100。Integration scope/subject 的 `{id}` 不存在时、grant 的 Room 不存在时返回 404 `not_found`。

**管理请求：**

**Integration 生命周期：**

| 方法与路径 | 严格 JSON body | 成功语义 |
|---|---|---|
| `POST /api/v1/integrations` | `{id,name}` | HTTP 201；`{"integration":{id,name,active,created_at,updated_at},"token":"<one-time plaintext>"}` |
| `PATCH /api/v1/integrations/{id}` | `{name?,"active"?}`；至少一个字段 | HTTP 200；`{"integration":{...}}`；`active:true→false` 会吊销已有 actor sessions |
| `POST /api/v1/integrations/{id}/token` | 无 body | HTTP 200；`{"integration":{...},"token":"<one-time plaintext>"}`；旧 token 与 actor sessions 立即失效 |
| `DELETE /api/v1/integrations/{id}` | 无 body | HTTP 200；`{"ok":true}`；级联语义见 3.5 |

创建 ID 重复返回 409 `conflict`；其它生命周期操作的 ID 不存在返回 404 `not_found`。所有成功变更写入 audit log；token/hash 永不进入 audit detail。

**映射与 grant 写请求：**

| 方法与路径 | 严格 JSON body | 成功语义与 HTTP 200 body |
|---|---|---|
| `PUT /api/v1/integrations/{id}/scopes` | `{adapter_id,scope_type,scope_id,room_id}` | upsert scope→Room；`{"scope":{integration_id,adapter_id,scope_type,scope_id,room_id}}` |
| `DELETE /api/v1/integrations/{id}/scopes` | 与 PUT 相同 | 仅在 `room_id` 与当前绑定一致时删除；`{"ok":true}` |
| `PUT /api/v1/integrations/{id}/subjects` | `{adapter_id,scope_type,scope_id,subject_id,principal_id}` | upsert subject→Principal；`{"subject":{integration_id,adapter_id,scope_type,scope_id,subject_id,principal_id}}` |
| `DELETE /api/v1/integrations/{id}/subjects` | 与 PUT 相同 | 仅在 `principal_id` 与当前链接一致时删除；`{"ok":true}` |
| `PUT /api/v1/rooms/{id}/grants/{principal_id}` | `{room_id,principal_id,"capability":"controller"}` | upsert grant；`{"grant":{room_id,principal_id,capability}}` |
| `DELETE /api/v1/rooms/{id}/grants/{principal_id}` | 与 PUT 相同 | 撤销存在的 grant；`{"ok":true}` |

- Integration 路径 `{id}` 必须是数据库中存在的 Integration；否则 404 `not_found`。
- scope 的 `room_id`、subject/grant 的 `principal_id` 以及 grant 的 Room 都必须已存在；不存在为 404。scope DELETE 也要求 body 指定的 Room 仍存在。
- DELETE scope/subject 的 body 值必须匹配当前记录；grant 的 `room_id/principal_id` 必须与路径一致。任何不匹配返回 409 `conflict`，不存在的绑定/链接/grant 返回 404 `not_found`。
- grant 当前唯一支持的 capability 是字面量 `"controller"`；其它值返回 400 `bad_request`。

### 6.5 外部加速资源

Acceleration 是由 `media_admin` 管理的持久机器资源，不从 `config.json` 读取。当前唯一
`kind` 为 `edgeone`，但 Core 的资源、凭据、容量和生命周期合同使用供应商无关命名。
资源创建时保持 disabled，并一次性返回 publisher、delivery 与 backend 三种 plaintext
credential；后续查询只返回 credential-configured/pending 布尔值。

| 方法与路径 | 合同 |
|---|---|
| `GET /api/v1/accelerations` | 列出资源，不含任何 token/hash |
| `POST /api/v1/accelerations` | 创建 disabled 资源；HTTP 201 返回资源和三种一次性 token |
| `GET /api/v1/accelerations/{id}` | 返回持久配置、健康状态与 credential flags |
| `PATCH /api/v1/accelerations/{id}` | 更新名称、端点、policy、水位或 enabled；启用前强制 readiness |
| `DELETE /api/v1/accelerations/{id}` | 仅允许删除 disabled 且不再拥有媒体或进行中工作的资源 |
| `GET /api/v1/accelerations/{id}/status` | summary、publisher、active progress、storage、当前 inventory scan、累计与最近 24 小时指标 |
| `GET /api/v1/accelerations/{id}/requests?state=&limit=` | 查询 `queued\|leased\|retry_wait\|cancel_requested\|ready\|evicted\|canceled` 请求；queued 返回 `pending_reason` |
| `GET /api/v1/accelerations/{id}/requests/{track_ref...}` | 查询单个请求的 lease、phase、进度、重试和取消状态 |
| `DELETE /api/v1/accelerations/{id}/requests/{track_ref...}` | 幂等取消；未认领任务立即 canceled，live lease 进入 cancel_requested |
| `POST /api/v1/accelerations/{id}/inventory/refresh` | HTTP 202；创建或复用当前完整 inventory scan |
| `GET /api/v1/accelerations/{id}/inventory/status` | 返回最后完整 storage 快照及当前/最近 scan |
| `POST /api/v1/accelerations/{id}/credentials/{purpose}/prepare` | 生成一次性 pending token |
| `POST /api/v1/accelerations/{id}/credentials/{purpose}/activate` | 验证并切换 pending token |

创建/更新的供应商无关字段为 `control_base_url`、`backend_base_url`、
`cache_mode`、`prefetch_horizon`、`prefetch_share_percent`、`lease_ttl_seconds`、
`upload_rate_bytes_per_second`、
`max_object_bytes`、`storage_budget_bytes`、`storage_high_watermark_percent`、
`storage_low_watermark_percent`、`inventory_interval_seconds` 与
`inventory_stale_after_seconds`。默认容量是 850 MiB，高/低水位是 95%/85%；
inventory 默认每 900 秒调度，超过 1800 秒没有完整观测即标记 stale。

加速资源是一个有自己预算的缓存，不是本地缓存的镜像。`cache_mode` 决定它的需求集合
从哪里来：

| 模式 | 需求集合 | 工作集 | 份额上限 |
|---|---|---|---|
| `prefetch` | 仅房间队列视界 | 房间数 × `prefetch_horizon`，**有上界** | 不生效，可用满预算 |
| `prefetch_and_heat` | 视界 + 缓存就绪事件 | 无界（播过多少首就有多少） | `prefetch_share_percent` |

`prefetch` 模式下需求有上界，因此不可能抖动；视界之外的曲目走源站 fallback。当预算
显著小于活跃曲目集时这是正确的模式——`prefetch_and_heat` 只有在预算能装下热集时才有
额外收益，否则会退化成"驱逐即重排"的重传循环。

`prefetch_horizon` 是从每个房间队列游标起算的曲目数，默认 2（当前曲目 + 下一首），
0 表示关闭待播钉住。视界内的曲目在请求侧获得认领优先级、在对象侧不可被 GC 驱逐。
钉住是 deadline 形状的：房间游标停止推进或进程崩溃时自行过期，不会永久占位；曲目
离开视界后钉住自然到期、落回热度池，而不是一放完就被回收。

`prefetch_share_percent` 是待播能占的预算上限，默认 20%，它同时是热度曲目的保底份额。
待播优先级高于热度常驻，但必须有上限——一次点满几十首的队列如果全部钉住，整个热集会被
冲光然后重传一遍，抖动只是换了个方向。**该值必须不大于 `storage_low_watermark_percent`**：
GC 的回收目标是低水位，而被钉住的部分它动不了，份额一旦越过低水位，GC 永远够不到目标。

`purpose` 仅允许 `publisher|delivery|backend`。pending publisher/delivery token
在切换窗口内可认证；backend activate 前必须通过受保护的 backend health。启用要求端点、
三种 current credential、control/backend health、正容量预算，以及 45 秒内带
`storage.inventory` 和 `object.delete` capability 的 publisher heartbeat；否则返回
409 `acceleration_not_ready` 并在 error 中给出 `problems`。

内部 `/internal/v1/accelerations/*` 路径仅接受 acceleration-scoped machine token。
调用方不能在 body 中选择 acceleration ID；Server 必须从 token 解析资源。publisher
config 可向已认证 adapter 返回解密后的 backend token、lease TTL、限速、对象上限和容量
水位。进度阶段只允许
`claimed→downloading→uploading→verifying→completing` 的非逆向转换，字节计数必须单调
增加；合法进度更新对 lease 做受资源 TTL 上限约束的续租。

取消是协作式 lease 合同。Core 一旦收到管理端 DELETE，后续 progress、reserve、complete
必须返回 409 `cancellation_requested`；publisher 也可轮询
`GET /internal/v1/accelerations/leases/{id}`。adapter 必须停止源文件读取/上传、清理临时
对象，并调用 `POST /internal/v1/accelerations/leases/{id}/cancel` 确认。若 publisher
失联，lease 到期回收会把 cancel_requested 请求终结为 canceled。取消记录和 attempt
历史必须保留，不得通过删除请求记录实现取消。

完整对象上传前，publisher 必须对 lease 调
`POST /internal/v1/accelerations/leases/{id}/reserve`，提交 opaque `locator` 与
`size_bytes`。Core 对 `(acceleration_id, locator)` 去重，并在同一事务中计算已记账对象与
未过期 reservation；超过高水位返回 507 `acceleration_storage_full`，同时按 LRU 使旧
candidate 失效并创建删除 job。adapter 通过
`POST /internal/v1/accelerations/deletions/claim` 领取 job，调用供应商 API 删除后再
complete；失败必须调用 fail 并给出有限 retry delay。

驱逐是缓存策略的正常结果，不是待重试的失败：GC 使 candidate 失效时，对应请求必须同时
进入 `evicted`，退出可认领集合与 `queued`/`retry_wait` 统计。只有真实需求——缓存就绪回调、
边缘 introspect 或进入预取视界——才会把它复活成 `queued`。若驱逐不写回需求侧，请求会在
candidate 被删的同一瞬间从 `ready` 翻回 `queued`，publisher 随即重传刚被回收的对象，形成
与预算无关的无限重传循环。

GC 的受害者集合排除被钉住的对象。凑不够低水位就凑不够——调用方会拿到 507，这比删掉
马上要放的那一首正确。认领顺序同样按钉住优先，否则马上要放的曲目会排在陈年请求之后。

回收在途期间——存在待删除 job 且占用尚未回落到低水位——Core 不派发新的 lease。否则
publisher 会先下载完整源再撞 507 或撞上处于 `deleting` 状态的同名 locator（409
`acceleration_storage_reserved`），每个失败周期浪费一次整源下载。

Core 以持久 inventory scan task 调度外部观测。adapter 先调用
`POST /internal/v1/accelerations/inventory/claim` 认领 scan，再向
`POST /internal/v1/accelerations/inventory` 分页提交同一 `scan_id`；所有页面进入独立
staging generation，只有最后一批 `complete:true` 才在事务中原子更新 observed bytes、
managed/observed/orphan/missing 对象数和 `observed_at`。快照描述的是 `observed_at` 时刻的
存储状态，分页扫描本身耗时可观，因此只有在 `observed_at` 之前就已落库的对象才由本次扫描
判决 missing；扫描窗口内完成的上传不在快照里属于预期，不得据此标记 missing。扫描失败必须调用
`POST /internal/v1/accelerations/inventory/{id}/fail`；失败或不完整 generation 不得覆盖
上一份完整快照。unknown locator 只作为 opaque orphan 记账，不会被 Core 解释成供应商
key。`storage.stale` 表示最后完整 `observed_at` 已超过资源的 freshness window；它不把
数据库查询时间冒充外部实时观测。删除到低水位前，GC 可以使被选中的 candidate 立即
unavailable；播放因此走既有源站 fallback，而不是继续引用待删对象。

## 7. 错误码

WS 错误仍使用 `{"type":"error","ref":"...","data":{"code","message"}}`；REST 使用 6.1 的 `{"error":{"code","message"}}`。`message` 面向诊断，可变；Client MUST 按 `code` 分支。

| code | REST 常用 HTTP | 含义 |
|---|---:|---|
| `unauthorized` | 401 | 未认证、session 无效/过期/吊销，或当前 Principal disabled/不存在 |
| `forbidden` | 403 | role、Room capability 或条目所有权不足；actor resolve 的 Principal 不可用 |
| `bad_request` | 400 | JSON/参数非法、严格请求出现未知字段、track_ref/source 格式错误 |
| `queue_full` | — | WS：达到房间 `max_queue` 上限；合同 A REST 当前返回 `conflict` |
| `quota_exceeded` | — | WS：超出 `policy.queue_limits`；合同 A REST 当前返回 `conflict` |
| `idempotency_required` | 400 | Integration actor 调 Room 写端点时缺少 `Idempotency-Key` |
| `invalid_binding_code` | 400 | external subject 绑定码错误、过期，或已被其他 external key 消费 |
| `not_found` | 404 | Room、条目、媒体、Principal、Integration 或映射不存在 |
| `conflict` | 409 | REST 当前状态冲突、重复资源、请求与已有绑定不匹配 |
| `idempotency_conflict` | 409 | 同一幂等 key 与操作被用于不同请求 body |
| `request_in_progress` | 409 | 同一幂等请求尚未完成；Client 可稍后重试 |
| `acceleration_not_ready` | 409 | 资源启用前的 endpoint/credential/health/publisher readiness 未满足 |
| `credential_not_pending` | 409 | activation 没有可切换的 pending credential，或 backend health 验证失败 |
| `acceleration_not_empty` | 409 | disabled acceleration 仍拥有媒体对象、lease、reservation 或删除工作 |
| `acceleration_storage_full` | 507 | 新对象会越过资源高水位；Core 已按 policy 排队回收，publisher 应稍后重试 |
| `acceleration_storage_unmanaged` | 409 | 资源缺少正数容量预算；启用 readiness 同样会拒绝 |
| `acceleration_storage_reserved` | 409 | 同一 opaque locator 已由另一个 live lease 预留；publisher 应稍后重试 |
| `request_ready` | 409 | 已完成的 distribution request 不允许取消 |
| `cancellation_requested` | 409 | 管理端已请求取消 live lease；publisher 必须停止并确认取消 |
| `inventory_scan_invalid` | 409 | inventory scan 不存在、租约过期、owner/observed_at 不匹配或状态错误 |
| `deletion_invalid` | 409 | 删除 job 不存在、已过期、状态错误或不属于提交 owner |
| `provider_error` | 502 | Provider 调用失败（附诊断 message） |
| `not_supported` | 501 | Provider 未实现可选能力（当前用于歌词） |
| `internal` | 500 | 服务端内部错误 |
| `rate_limited` | 429 | 同一来源 IP 的管理员口令或 Room guest credential 错误探测超过窗口阈值；不携带管理员口令的普通 guest 登录、Room 身份免密路径不计入相应探测 |

## 8. 明确不做的

- 投票切歌、DJ 模式（policy_json 可扩展，不实现逻辑）
- 曲内播放进度持久化（当前曲目本身保留在 DB 的队列游标里，重启从它的开头续播；
  `position_ms` 不落库）
- 队列事件的增量 diff 下发
- 账号密码登录（guest + 全局管理员口令 + OIDC 已覆盖当前认证需求）

## 9. 客户端实现清单

一个新 Client（官方 WebUI/CLI/Agent，或 AstrBot Plugin、Game Mod）先按 §0 选择协议面。所有展示和外部命令语法都留在 Client：

- 交互式 WebUI/CLI 通常用 REST 做目录/管理，用 WS 加房订阅实时状态并发命令。
- 收听 Agent 用 WS 快照驱动播放器；只有要接受管理员远程音量/换房时才实现 player plane。
- Integration Client 的最小流程是：解析外部事件 → 用稳定的 adapter/scope/subject 调 3.6 resolve → 取 `default_room_id` 或 Client 自己选择 Room → 用 `actor_token` 调 6.3 标准 REST（写请求携带由平台事件 ID 派生的 `Idempotency-Key`）→ 把领域 JSON 渲染成聊天/游戏内容。需要实时订阅时，再用同一 `actor_token` 做 WS session 认证。
- Client 不得向不存在的 `/im/v1` 发请求，也不得期待 Server 理解诸如“点歌 xxx”的原始文本。

### 9.1 WS 连接时序

```
1. 连接 WebSocket /ws/v1
2. 校时：3–5 轮 ping/pong（§2.1），得 offset
3. auth：guest name/password，或 {"session_token": "..."}（OIDC/actor token 均可）
4. room.join（§4.2）→ 等 room.joined；普通 guest 按 Room 当前模式填写 `password`，免密身份可省略。same-room Integration actor 无需先读取 access code；该码用于分享给走 fallback 的 guest
5. 分别接收 playback / revisioned queue.snapshot / radio / listeners 当前状态；按 type 分派，不依赖固定消息条数
6. 进入事件循环：playback/radio/listeners 用最新完整状态覆盖；queue.patch 收齐后按 revision 原子应用，mismatch 立即 queue.sync
```

只做一次性命令/查询的 REST Client 无需建立 WS、校时或 join；需要实时订阅时才执行上述流程。

### 9.2 播放渲染（仅收听型客户端）

```
每次 playback.changed：
  current 为 null          → 停止播放
  track_ref 变了           → 用 stream_url 加载新媒体
  playing 变了             → 同步暂停/恢复
  should_be < 0            → 起播提前量窗口：装载后保持暂停预缓冲，
                             到 updated_at + (-position_ms) 时刻再开声，
                             此期间跳过校偏（§2.2）
  之后执行校偏：
    should_be = position_ms + (server_now() - updated_at) * rate  (playing)
    drift = 本地播放位置 - should_be - drift_baseline
    |drift| > 150ms        → seek 到 should_be + drift_baseline，基线作废重学
    ≤ 150ms                → 不动
周期任务：每 1s 校偏一次；每 60s 重校时一次（§2.2）
```

纯 seek，不调速：变速会改变音高与听感，代价高于一次 150ms 以上的跳转。
`drift_baseline` 是输出链路延迟造成的读数偏差，学习与使用细则见 §2.2。


#### 9.2.1 Headless Agent 连接时序

```text
1. 连接 WebSocket /ws/v1
2. 校时：3–5 轮 ping/pong
3. auth：{"player_key":"yzp_…"}
4. player.hello：只上报 device/version/caps，不上报 Player ID
5. 已分配 Room → 等 `room.joined` 与 `playback.changed`；未分配 → 保持在线等待。Player 不收 queue/radio/listeners 快照
6. player.state 上报设备实际 volume/muted；按 playback.changed 驱动渲染
```

Agent 不调用 `room.join`。管理员可以在 Agent 离线前先创建 Player 并绑定 Room；Agent 首次上线即自动进入目标 Room。


### 9.3 拉流

- `GET {server}{stream_url}`（stream_url 已含 ticket 与版本前缀，直接拼接服务器基址即可）。
- 支持 Range；票据 5 分钟内可复用，曲目播完即失效。
- 401 = 票据失效/缺失：等待下一条 `playback.changed` 获取新 `stream_url`，不要自行编造。

### 9.4 断线与重连

- 普通 WS 连接和 Room listener 关系是内存态：断线即离房。普通 session 持久化且可在过期/吊销前复用；Integration `actor_token` 只有效 5 分钟，过期后须重新 resolve。
- 普通 WS 重连后 MUST 重新校时、以仍有效的 session token 认证并 join，以取得新的 revisioned `queue.snapshot` 基线与其它当前状态；token 已失效则先重新登录/resolve。
- Headless Agent 重连后 MUST 用 Player key 重新认证并发送 `player.hello`；服务端按持久 Room 分配自动加入，不得发送 `room.join`。
- 服务端会主动断开发送缓冲满、Player 停用/删除、key 轮换或同 ID 被新连接替代的连接。客户端 SHOULD 指数退避重连（如 1s/2s/5s/10s/30s 封顶），稳定运行后重置退避。

### 9.5 其他纪律

- MUST 忽略响应 JSON 中未知字段（版本纪律，§0）；不得据此给严格请求擅自增加字段。
- WS MUST 用 `ref` 匹配请求与响应（§4.3）；广播消息无 `ref`。REST 按 HTTP 请求本身匹配响应。
- SHOULD 假定任何操作都可能失败：WS 读取 `error.data.code`，REST 读取 `error.code`（§7），均按 code 而非 message 分支。
- 服务器位置推算的唯一依据是 playback 五元组 + offset（§2.2）；不要信任本地计时外推超过校偏间隔。

### 9.6 参考实现

仓库 `internal/client/` 是本协议 WS、无状态 Room REST、Player 生命周期与 Integration actor/管理 API 的 Go 参考实现；`cmd/yuzu-agent/`（MPV 渲染代理）与 `cmd/yuzu-cli/`（控制端）是两类 Client 的最小完整示例。

Go Client 提供完整生命周期、管理查询和幂等写 helper，可直接供第一方 CLI 或外部 Integration Client 复用：

```go
caps, err := client.RESTRoomCapabilities(ctx, server, actorToken, roomID)
created, err := client.RESTCreateIntegration(ctx, server, roomAdminToken, id, name)
rotated, err := client.RESTRotateIntegrationToken(ctx, server, roomAdminToken, id)
integrations, err := client.RESTListIntegrations(ctx, server, roomAdminToken)
scopes, err := client.RESTListIntegrationScopes(ctx, server, roomAdminToken, integrationID)
subjects, err := client.RESTListIntegrationSubjects(ctx, server, roomAdminToken, integrationID)
principals, err := client.RESTListPrincipals(ctx, server, roomAdminToken, query, limit)
grants, err := client.RESTListRoomGrants(ctx, server, roomAdminToken, roomID)
player, err := client.RESTCreatePlayer(ctx, server, roomAdminToken, id, name)
_, err = client.RESTBindRoomPlayer(ctx, server, roomAdminToken, roomID, player.Player.ID)

writeCtx := client.WithIdempotencyKey(ctx, platformEventID)
_, err = client.RESTRoomQueueAdd(writeCtx, server, actorToken, roomID, trackRef)
```

Integration 的 `created.Token` / `rotated.Token` 与 Player 的 `player.Key` 只应写入 secret store，不得记录日志。`caps.Controller` 是服务端 Authorizer 的最终判断；管理查询 DTO 可用于选择目标后调用 scope/subject/grant/Player helpers，无需读取服务端配置或 secret。
