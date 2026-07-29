# EdgeOne 旁路分发设计与部署

本文描述公网部署时可选的 EdgeOne 媒体分发模块。它不是 `/stream/v1`
的 v2，也不是所有部署的必选依赖。

## 目标与边界

- 局域网部署继续由 `yuzu-server` 直接提供 `/stream/v1/{ref}`，行为不变。
- 公网部署可把同一路径挂到 Edge Function；客户端协议、Range 语义和播放器代码不变。
- Provider 始终由 Yuzu 所在服务器拉取，避免 Provider 看到频繁变化的边缘 IP/地域。
- EdgeOne 只接管已经被 Yuzu 拉取的媒体字节分发。
- EdgeOne SDK、Blob key 和项目凭据不得进入 `yuzu-server`。
- EdgeOne 故障、对象尚未就绪或能力不兼容时，必须能回退到现有直出路径。

## 2026-07-29 能力实测

测试环境：EdgeOne CLI 1.6.16、`@edgeone/pages-blob` 0.0.13、Makers 本地调试与
EdgeOne production deployment。

### 已证实

1. Edge Cache 可以缓存完整 `200` 对象，并根据后续请求的 `Range` 自动返回：
   - 合法范围：`206` 和正确的 `Content-Range`；
   - 后缀范围：正确的 `206`；
   - 越界范围：`416`；
   - 1 MiB 与 16 MiB 完整对象均已验证。
2. Blob 凭据会注入 Cloud Function；当前远程 Edge Function 调试环境未获得 Blob 凭据。
3. Cloud Function 可完成 Blob put/get/metadata/delete，并可签发官方预签名 PUT URL。
4. Yuzu adapter 可以直接 PUT 到 Blob，媒体字节无需经过 Cloud Function；签名绑定的
   `Content-Type` 不匹配时正确返回 `403`。
5. 4 × 256 KiB 和 16 × 1 MiB Blob 可顺序拼成一个普通文件；完整响应、跨 chunk
   Range、后缀 Range 与 `416` 均逐字节验证正确。
6. Edge/Cloud 运行时拼接必须顺序使用 `ReadableStream.pipeTo()`。手工
   `writer.write()/writer.close()` 在 Edge 远程调试中会稳定丢失尾部数据。
7. Blob SDK 0.0.13 的 `get({type: "stream"})` 并非端到端流式：SDK 先执行
   `response.arrayBuffer()`，再把完整 `Uint8Array` 包成流。因此每个 Blob 对象必须控制大小。
8. 实验性 GET 签名已成功：Cloud Function 只返回一个短期 Blob GET URL，客户端携带任意
   `Range` 直接访问 Blob 域名时，可得到标准 `206`。媒体字节不经过 Yuzu 或 Cloud Function。
9. Blob 域名自身存在边缘缓存，但不同请求的 `eo-cache-status` 在 MISS/HIT 间变化，不能依赖
   某一节点必然命中。MISS 也只会回 Blob/COS，不会回 Yuzu。
10. 远端 production deployment 已验证完整链路：Edge Function 调用同项目 Cloud Function
    control/backend，再从 Blob 拉取 16 MiB 对象；完整响应 SHA-256 一致，普通 Range、
    后缀 Range 和越界 `416` 均正确。
11. 预签名 PUT 可以附带未参与签名的 `Cache-Control: public, max-age=31536000, immutable`，
    Blob metadata 会正确保存该值。
12. 生产 Cloud Function 同步响应上限已精确测得为 6 MiB：6 MiB 正常，6 MiB + 1 byte
    被平台转换为 `413`。Cloud Function 不可承担整轨数据面。
13. 生产 Edge Cache 可直接消费 Blob 的 16 MiB 流，并用稳定 synthetic key 缓存；后续
    Range 由 `cache.match` 自动返回正确 `206`。
14. P1 正式 Makers 工程已完成 production 部署；Cloud control bridge 能读取远端环境变量，
    backend health、64 KiB PUT、强一致 metadata、signed GET Range、delete 均再次验证通过。

### 尚未证实或存在风险

- Makers 本地调试中的 outbound fetch 探针会超时，但 production deployment 已证明真实
  Edge Function outbound fetch 可用；应把该现象视为本地调试拓扑限制。
- GET URL 不是 Blob SDK 0.0.13 的公开 API。当前实验利用 SDK 运行时内部对象取得临时
  STS、Blob 域名和真实 key，再按 COS V5 算法签名。升级 SDK 时可能失效。
- Cloud Function 的 6 MiB 响应限制已经确认；持续时间、并发和计费仍需按套餐确认。
- 尚未使用真实 `<audio>`/MPV 做播放、seek 和断线续传测试；HTTP 字节与 Range 语义已验证。
- Blob 单对象文档上限为 25 MB，不能假设所有 Bili/长音频都能作为单对象保存。
- candidate 现以 `acceleration_id + track_ref` 持久保存；容量、LRU、orphan/missing
  reconciliation 与物理删除已实现，但 Provider 同一引用的内容变化仍不会主动触发重发。
- EdgeOne CLI 1.6.16 的 `makers env set` 存在参数与异步等待缺陷，可能以状态 0 静默退出；
  production 应通过 Makers 控制台或已修复版本写变量，并在部署日志确认变量已被拉取。

## 推荐拓扑

```text
局域网：
Client ── /stream/v1 ──> yuzu-server

公网（可选）：
Client ── 主域名 /stream/v1 ──> EdgeOne 站点级 Edge Function
                                      │
                                      ├─ Makers Cloud control bridge
                                      │      ├─ ticket/candidate introspection ──> yuzu-server
                                      │      └─ 短期 Blob GET 签发
                                      ├─ 媒体字节：Range GET ───────────────────> EdgeOne Blob/COS
                                      └─ fetch(event.request) ──────────────────> 站点标准回源

yuzu-server cache ──localhost/read-only source──> yuzu-edgeone adapter
yuzu-edgeone adapter ──backend PUT URL ──presigned PUT──> EdgeOne Blob
```

模块职责：

- `yuzu-server`
  - 继续负责 Provider 拉取、本地 cache、现有 stream ticket 和直出回退；
  - 持久管理 acceleration、机器凭据、lease/candidate、publisher heartbeat 和上传进度；
  - candidate 的 locator 对 Core 是不透明字符串，不理解 Blob key。
- `yuzu-edgeone`
  - 独立可选进程；使用最小 bootstrap credential 从 Core 读取托管配置；
  - 独占租约、限速上传、失败重试、metadata 校验和 candidate 提交；
  - 上报存活、下载/上传进度和错误，不把 EdgeOne SDK 写进 Core。
- Cloud Function
  - `yuzu-edge` 作为同项目控制桥，持有 Core origin/token 并保持 ticket 校验语义；
  - `yuzu-blob` 受独立 token 保护，批量签发 PUT URL、校验 metadata 和执行限定删除；
  - 隔离封装实验性短期 GET signer；
  - 官方 SDK 读取只可作为不超过 6 MiB 的小 Range fallback，不能代理完整媒体。
- EdgeOne 站点级 Edge Function
  - 在 Yuzu 主域名上通过 `/stream/v1/*` URL path 触发规则接管请求；
  - 使用代码顶部的 Makers control absolute URL introspect ticket；
  - ready 时从 Blob 流式响应；
  - disabled、未 ready、control/backend/Blob 故障时通过 `fetch(event.request)` 走站点标准回源；
  - 不接触 Provider，也不持有 Core、backend 或 EdgeOne 长期凭据。

## 对象布局

建议使用内容寻址，而不是直接使用 `track_ref`：

```text
media/{sha256}/object
media/{sha256}/manifest.json
media/{sha256}/chunks/000000
media/{sha256}/chunks/000001
...
```

manifest 至少包含：

```json
{
  "version": 1,
  "size": 16777216,
  "content_type": "audio/mpeg",
  "etag": "content-version",
  "layout": "object",
  "chunk_size": 0,
  "chunk_count": 0
}
```

使用内容 hash 可以避免 Provider ID 复用、音质配置变化和对象覆盖造成的缓存串流。

## 两种媒体布局

### 完整对象（首选简单路径）

- 已知完整文件不超过约 24 MiB 时，上传一个 Blob 对象，给 25 MB 平台上限留余量。
- Edge Function 将客户端 Range 原样传给短期 GET URL，并流式转发响应。
- Blob/COS 原生完成 Range；不需要 Functions 拼接，内存占用和故障面最小。
- 若采用 302 直接跳转，效率更高，但后续 Range 会绕过 Yuzu ticket introspection，可能弱化
  “切歌立即失效”语义。默认应使用 Edge Function 代理；重定向只作为显式实验开关。

### 分块对象（大文件与渐进发布）

- 推荐初始 chunk 大小 1–2 MiB；即使走 Cloud SDK fallback，每次被完整聚合的对象也很小；
  同时能把 seek 的无效读取控制在小范围。
- Edge Function 解析单 Range，只请求相交 chunk；首尾切片后按顺序 `pipeTo`。
- Cloud Function signer 应一次返回所需 chunk 的 URL，避免每块一次函数调用。
- 第一阶段只在整个 manifest ready 后发布；后续再考虑“连续前缀已就绪”的渐进播放。

## 冷启动与带宽策略

上传 Blob 本身仍会消耗服务器一次上行，因此不能无节制抢满 3 Mbps：

- 当前曲目没有 ready candidate：立即走现有直出，同时后台发布。
- 下一曲已由 Room 预取：本地 cache 完成后优先上传，争取在切歌前 ready。
- adapter 当前默认固定限速 1.5 Mbps；根据当前直出连接数动态降低或暂停留待 P3。
- 同一内容只允许一个 publish lease；其他任务等待 candidate，避免重复上传。
- 发布失败不影响播放，只记录状态并继续走源站。

这会把公网带宽从“每个听众重复发送整轨”变为“每个新内容最多上传一次”；热门或多人同时
播放的收益最大，首次冷门曲目的体验与当前实现相同。

## Cache API 的位置

第一阶段不要把 Edge Cache 设为正确性依赖：

- Blob 域名已经提供边缘接入，MISS 也不会消耗 Yuzu 带宽；
- Cache API 内容只在当前数据节点有效，不会自动复制；
- `cache.put` 不接受 `206`，而媒体客户端经常从 Range 请求开始；
- 为填满整轨缓存而额外请求一个完整 `200` 会增加 Blob 流量和复杂度。

可在第二阶段增加稳定 synthetic key 的 chunk cache。只缓存完整 chunk 的 `200`，后续由
`cache.match(Request{Range})` 自动裁剪；不要使用带临时签名参数的 URL 作为 cache key。

## 安全要求

- backend 只允许固定 store/prefix，禁止调用者提供任意 COS key、域名或方法。
- publisher adapter、Edge Function 与 Cloud Function 间使用独立机器凭据。
- PUT/GET URL 短时有效，禁止写日志；GET TTL 应与 stream ticket 风险窗口匹配。
- Edge Function 必须先 introspect ticket，再获取 Blob URL。
- GET signing 放在独立适配层，启动时做 SDK 兼容检查；失败时自动切回源站。
- candidate 必须携带不可变内容版本，防止旧 URL/旧缓存映射到新媒体。

## 实现 Todo

### P0：部署级能力门槛

- [x] 部署远端项目，验证 Edge Function 能 fetch 同项目 Cloud Function。
- [x] 验证 Edge Function fetch 短期 Blob GET URL，并无损传输 16 MiB 完整响应与 Range。
- [ ] 使用真实 `<audio>`/MPV 验证播放、seek、断线续传和取消请求。
- [ ] 验证多 Range，并选择拒绝或明确实现 multipart 响应。
- [x] 测得 Cloud Function 同步响应上限为 6 MiB。
- [ ] 确认 Cloud Function 计费，并确认官方是否计划提供 `createDownloadUrl`。
- [x] 验证预签名 PUT 携带长期 `Cache-Control` 与 Content-Type。
- [ ] 验证是否需要显式 Content-Length，以及不同上传客户端的一致性。
- [ ] 用不同地域/网络重复请求，记录 Blob 缓存行为，但不把 HIT 当正确性条件。

### P1：完整对象 MVP（已完成）

- [x] 定义通用 acceleration lease/candidate/introspection 数据模型与内部 API。
- [x] 实现 `yuzu-edgeone` adapter 骨架、配置、鉴权、限速器和本地状态库。
- [x] Cloud Function 实现受保护的批量 PUT URL。
- [x] adapter 对不超过 23 MiB 的完整缓存文件执行 PUT、校验 metadata、提交 ready candidate。
- [x] Edge Function 保持 `/stream/v1`，完成 ticket introspection、Blob Range 代理和源站回退。
- [x] GET signing 提供 SDK 兼容检查、120 秒 TTL，并在不可用时回退现有源站。
- [x] 增加发布成功率、上传字节、回退次数、ready 延迟、Blob 响应时间指标。

### P1 生产化：托管资源、观测与主域名接入（已实现）

- [x] acceleration 改为 `media_admin` 可创建、停用和管理的持久资源，移除 Server JSON 配置。
- [x] publisher/delivery/backend 使用独立凭据；出站 backend secret 经 `secret_key` AES-GCM 加密。
- [x] adapter 从 Core 读取 backend URL、token、限速、对象上限和 lease policy。
- [x] 增加 publisher heartbeat、下载/上传字节、phase、错误、attempt history 与有界续租。
- [x] 增加 status/requests 管理 API、健康探测、fallback 原因和最近 24 小时指标。
- [x] 实现 staged credential prepare/activate。
- [x] 公开 stream 函数迁移为可直接粘贴的站点级 Edge Function，Makers 只保留控制面。
- [ ] 在真实主域名 Dashboard 部署函数与触发规则后，完成 `<audio>`/MPV 端到端回归。

### P2A：完整对象生命周期与容量治理（已完成）

- [x] Core 使用供应商无关的 acceleration/backend/delivery 合同；EdgeOne key 对 Core 保持 opaque。
- [x] 上传前按 lease 原子预留容量；同 content-addressed locator 只记账一次。
- [x] 持久保存 object/refcount/access time、reservation、inventory snapshot、deletion job 与 reconcile status。
- [x] adapter 周期性执行 Blob 强一致 inventory，报告 observed bytes、orphan 与 missing 对象。
- [x] 高水位触发 LRU candidate 失效和删除 job，回收到低水位；删除失败有租约与 retry。
- [x] readiness 要求容量预算和 `storage.inventory`/`object.delete` capability；管理 status 暴露容量压力。
- [x] adapter 重启会释放持久化的 live lease；同 locator 预留串行化，重复提交与上传中断通过 ready/orphan reconciliation 收敛。

### P2B：分块与内容变化

- [ ] 实现 1–2 MiB byte-exact chunks、manifest、批量 GET signing 和顺序 `pipeTo`。
- [ ] 对缺块、manifest 不一致、对象被回收返回可观测错误并回退源站。
- [ ] 检测同一 `track_ref` 的内容版本变化，原子切换 candidate 并回收旧内容对象。
- [ ] 评估渐进发布，只有连续前缀与请求 Range 均已 ready 时才走 Blob。

### P3：可选优化

- [ ] 在真实数据证明有收益后加入节点级 chunk Cache API。
- [ ] 根据当前直出连接数与 Room 预取窗口动态调整上传限速。
- [ ] 评估对 ready 的完整对象采用短 TTL 302；默认仍保留代理以维持撤销语义。

## Go/No-Go

核心 Blob Go/No-Go 已通过：远端 Edge Function 可以稳定 fetch Cloud signer 与 Blob signed GET，
并传输 16 MiB 长响应。P1、生产化和完整对象容量生命周期代码已实现；在面向真实用户启用前，
仍需把站点函数粘贴到主域名 Dashboard、配置 `/stream/v1/*` 触发规则，并补
`<audio>`/MPV 的播放、seek、断线续传测试。

Cloud Function 官方 SDK 拼接不能作为整轨 fallback，因为响应超过 6 MiB 会被平台返回
`413`。整轨 fallback 必须回现有 Yuzu 源站，或继续走 Edge Function → signed Blob GET。

## 部署入口

- acceleration CRUD、Core/Makers 凭据、adapter bootstrap、站点函数粘贴与触发规则见
  [`deploy/edgeone/README.md`](../deploy/edgeone/README.md)。
- adapter 最小示例配置见 [`edgeone.example.json`](../edgeone.example.json)。
- 站点函数代码见 [`deploy/edgeone-site/stream.js`](../deploy/edgeone-site/stream.js)；
  粘贴前只需修改顶部非秘密 `CONTROL_ORIGIN` 常量。
