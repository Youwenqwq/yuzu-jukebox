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

## config.json 生产注意

```jsonc
{
  "addr": "127.0.0.1:8080",      // 反代模式下只监听回环
  "admin_password": "…",          // 强口令；持有即全管理员
  "secret_key": "…",              // 首次启动自动生成并回写；凭据加密主密钥
  // ...
}
```

- `secret_key`：**丢失 = credentials 表全部不可解密**（ncm/bili cookie 需重登）。
  备份数据目录时必须连同 config.json 一起备份。
- `config.json` 与 `data/` 目录建议 `chmod 600` / `chmod 700`：secret_key、
  session 持久化数据、凭据都在其中。
- `admin_password` 与 OIDC 的关系：guest + admin_password 仍是最高权限后门；
  组织部署建议留着做 break-glass，但口令强度要够。

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
