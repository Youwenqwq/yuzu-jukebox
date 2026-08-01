# 部署指南

yuzu-server 本身是纯 HTTP 服务。**生产环境请放在反向代理之后终结 TLS**，
不要在公网裸跑（session token 与出流 ticket 都是明文传输的凭证）。

## 反向代理

### Caddy（推荐，最简）

```
jukebox.example.org {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy 自动签发/续期证书，且默认正确处理 WebSocket 升级（无需额外配置）。

### nginx

```nginx
server {
    listen 443 ssl;
    server_name jukebox.example.org;

    ssl_certificate     /etc/letsencrypt/live/jukebox.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/jukebox.example.org/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

        # WebSocket 穿透（/ws/v1 必须）
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";

        # 出流是长连接大文件，关闭缓冲、放宽超时
        proxy_buffering off;
        proxy_read_timeout 3600s;
    }
}
```

要点：

- **WebSocket 升级头**（`Upgrade`/`Connection`）必须有，否则 `/ws/v1` 握手 400。
- **WS 空闲超时**：客户端有 ping/pong 校时（分钟级），默认 60s 的代理超时通常够用；
  若见莫名断连，调大 `proxy_read_timeout`。
- **出流路径** `/stream/v1/` 是边下边播的长响应，关闭代理缓冲（Caddy 默认不缓冲）。

## 公域部署

公域实例按以下清单部署：

- 将 `"admin_password"` 设为 `""`。`internal/auth/auth.go` 的 `GuestAuth`
  只有在配置口令非空且提交值命中时才追加 `room_admin` 与 `media_admin`；
  留空即关闭 guest 口令升级。
- 管理员一律经 OIDC 登录，并用 `oidc.role_mapping` 将 IdP 角色映射为
  `room_admin` / `media_admin`；不要把全局口令当作公网 break-glass 后门。
- 在反向代理终结 TLS，并在边缘按真实客户端 IP 对
  `POST /api/v1/auth/guest`、WebSocket 建连等认证入口增加限流。服务端不信任
  `X-Forwarded-For`，内建限制按 TCP 直连地址计数；反代部署时多个公网客户端
  会共享代理地址的服务端桶，因此边缘限流仍然必要。
- 服务端对 REST guest 登录与 WS `auth` 的非空错误口令探测启用内存限流：
  同一来源在 10 分钟内前 10 次仍按普通访客认证，后续带口令请求在窗口内返回
  `rate_limited`（REST 为 HTTP 429）；不带口令的访客认证永不受限。重启会清空计数。

仅在受控的私域（LAN）部署中，才可按需要保留强 `admin_password`；即使如此，
OIDC 仍是多人管理与角色审计的推荐方式。

## 可选 EdgeOne 媒体旁路

公网实例若受源站上行带宽限制，可选择部署 `yuzu-edgeone`、Makers 控制面与
[`deploy/edgeone-site/stream.js`](../deploy/edgeone-site/stream.js)。该模块只接管已经由
Yuzu 拉取并缓存的媒体文件分发；Provider 拉流、现有 `/stream/v1` 协议和局域网部署行为
都不改变。

在主域名所属 EdgeOne 加速站点中创建站点级 Edge Function，粘贴该文件代码，并为
`/stream/v1/*` 配置 URL path 触发规则。REST、WebSocket、管理接口和 SPA 仍按站点原有
规则回源。Makers 仅保留 control/backend Cloud Functions。Core 使用 850 MiB 默认预算及
95%/85% 水位；adapter 负责 Blob inventory、orphan/missing reconciliation 和删除 job。
外部加速存储能力较小时应调低低水位（`storage_low_watermark_percent`），按可用空间留足
余量：低水位是 GC 的回收目标，被钉住的预取对象不可被驱逐，钉住份额一旦越过低水位，
GC 永远收不到目标，容量压力会反复出现。
具体资源创建、凭据、容量字段、触发规则与健康检查见
[EdgeOne 旁路分发设计与部署](edgeone-distribution.md)。

## config.json 生产注意

```jsonc
{
  "addr": "127.0.0.1:8080",      // 反代模式下只监听回环
  "admin_password": "",             // 公域必须留空；私域按需设置强口令
  "secret_key": "…",              // 首次启动自动生成并回写；凭据加密主密钥
  // ...
}
```

- `secret_key`：**丢失 = credentials 表全部不可解密**（ncm/bili cookie 需重登）。
  备份数据目录时必须连同 config.json 一起备份。
- `config.json` 与 `data/` 目录建议 `chmod 600` / `chmod 700`：secret_key、
  session 持久化数据、凭据都在其中。
- `admin_password` 与 OIDC 的关系：公域必须留空并通过 OIDC `role_mapping`
  授予管理角色；仅受控私域可按需保留强口令。

## 进程管理（systemd 示例）

```ini
[Unit]
Description=yuzu-jukebox server
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/yuzu-jukebox
ExecStart=/opt/yuzu-jukebox/yuzu-server -config /opt/yuzu-jukebox/config.json
Restart=on-failure
RestartSec=3
# 依赖的 Provider API sidecar（NeteaseCloudMusicApi / bilibili-api）另行部署

[Install]
WantedBy=multi-user.target
```

重启语义：队列持久化（自动续播队首）、会话持久化（客户端不掉登录）、
电台绑定为运行时状态（需重新 `radio play`）。

## 备份

最小备份集：`config.json` + `data/yuzu.db`（+ `data/media/` 若使用本地上传）。
缓存目录 `data/cache/` 可丢（自动重建）。
