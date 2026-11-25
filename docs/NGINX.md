# 域名与 Nginx

## 两种 HTTPS 入口

| 部署 | public_url | 节点 TLS 校验 |
| --- | --- | --- |
| 直接访问 Hub | `https://IP:9443` | 新链接的 auto 模式可验证 Hub 自签证书指纹 |
| Nginx 终止域名 TLS | `https://ssh.example.com` | 新链接的 auto 模式验证域名、有效期和系统 CA 信任链 |

`public_url` 只声明外部地址，不安装 Nginx、不申请域名证书、不把 Hub 改为 HTTP。Hub 的默认监听仍是 HTTPS `:9443`。浏览器与节点经 Nginx 的 HTTPS 443 访问控制台，Nginx 再用 HTTPS 连接本机 Hub。SSH 客户端继续使用原来分配的公网 SSH 端口。

## 配置已有 Nginx

准备域名解析和有效的域名证书，在现有 `server` 中保留 `listen 443 ssl`、`server_name` 与证书配置。`ssl_certificate` 应使用包含中间证书的完整证书链，例如 ACME 的 `fullchain.pem`。

使用安装后的程序生成 `location /`：

```bash
sudo nodebridge nginx --data-dir /var/lib/nodebridge/hub
```

默认输出如下；自定义监听端口或 IPv6 回环时程序会调整后端地址：

```nginx
location / {
    proxy_pass https://127.0.0.1:9443;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_ssl_server_name on;
    proxy_ssl_name ssh.example.com;
    proxy_ssl_verify off;
    proxy_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_redirect off;
}
```

用这段内容替换现有 `location /`，避免重复定义。`Host $http_host` 保留外部域名与非默认端口，使浏览器写请求的 Origin 校验正常。WebSocket Upgrade 头与长连接超时用于节点隧道，配置依据 [Nginx 官方 WebSocket 说明](https://nginx.org/en/docs/http/websocket.html)。

`proxy_ssl_verify off` 仅用于生成器限定的**本机回环后端**，以接受 Hub 的自签证书；不会关闭节点到域名的证书校验。生成器拒绝非回环后端。节点仍必须通过域名 CA 校验或精确的配对指纹校验。

```bash
sudo nginx -t
sudo systemctl reload nginx
sudo nodebridge doctor --data-dir /var/lib/nodebridge/hub
```

`doctor` 检查配置的公网地址 TLS、`/api/health` 的 Hub 响应，以及未认证访问 `/api/connect` 的预期 401；能发现错误的 HTTP 后端、错误站点与 API 路径。它不使用节点凭证，实际 WebSocket 建连由节点完成。安装脚本也会执行检查，失败时明确提示；不会擅自覆盖现有 Nginx 配置或停止已安装的 Hub。

## 将临时的 :9443 入口切回域名 443

先保留已修正的 HTTPS `proxy_pass` 与有效的 Nginx 域名证书。旧程序先按 [README](../README.md#命令升级) 更新 hub 与每台已配对 node，再按下面的命令使用 `--reconfigure` 更新地址。普通升级无需 URL 参数；地址迁移会重启对应服务并断开 SSH 会话。无需移除节点、重新兑换邀请或更换原 SSH 端口。

已经安装 v0.4.0 后，仅修改连接地址也可以使用下面的命令。将示例域名替换为实际域名：

```bash
# VPS
sudo systemctl stop nodebridge-hub
sudo -u nodebridge nodebridge hub --data-dir /var/lib/nodebridge/hub --public-url https://ssh.example.com --reconfigure --init-only
sudo systemctl start nodebridge-hub

# 已配对的节点
sudo systemctl stop nodebridge-node
sudo -u nodebridge nodebridge node --data-dir /var/lib/nodebridge/node --hub-url https://ssh.example.com --tls-mode auto --reconfigure --init-only
sudo systemctl start nodebridge-node
```

修改通过原子替换保存；旧配置备份为同目录 `config.json.bak-时间戳`，权限为 `0600`。节点 ID、凭证、Hub 指纹、SSH 端口、暂停状态以及 Hub 证书/私钥保持原值。只改变 Hub 地址不会自动改变已配对节点的地址，需逐台执行节点命令。

尚未配对的节点只需更新 node，再使用新版 hub 生成的链接配对。旧链接没有 TLS 模式字段，仍使用严格指纹校验；不要继续复制旧链接来连接 Nginx。

## TLS 模式与证书续期

- `auto`：v0.4.0 新链接明确携带此模式。接受正常的域名/有效期/系统信任链校验，或链接给定的 Hub 证书指纹及有效期校验。Nginx 的域名证书与 Hub 内部证书可以不同；正常 CA 证书续期后节点可直接重连。
- `pinned` 或旧配置缺少字段：只接受原 Hub 指纹和有效期；升级程序不会默默放宽已有节点的信任方式。
- `system`：仅接受系统 CA 与域名校验，不回退到 Hub 自签指纹。已配对节点可使用同样的配置命令，将 `--tls-mode auto` 改为 `--tls-mode system`。

过期证书、域名不匹配且不匹配 Hub 指纹的证书、无效 CA 且不匹配指纹的证书均被拒绝。API 重定向也被拒绝，避免把邀请令牌发送到错误站点。系统需有可用的 CA 根证书；Nginx 使用完整证书链。NodeBridge 不负责申请或续期 Nginx 的证书。

## 常见错误

| 现象 | 原因与处理 |
| --- | --- |
| `http://127.0.0.1:9443` 返回 400，而 HTTPS 返回 200 | 正常的 HTTPS 服务行为；将 Nginx 后端改为 `https://`，不必改 Hub 监听协议 |
| IP:9443 可访问，域名 400/502 | 检查 HTTPS 后端、监听端口及 Nginx 对外证书；运行 `doctor` |
| 网页正常，节点配对报指纹不匹配 | 老链接/节点只校验内部证书；更新两端并用新链接，或对已配对节点显式切换 auto |
| 网页和配对正常，节点连接失败 | 检查 `/api/connect` 的 WebSocket Upgrade 头、Host 与代理路径 |
| 域名证书续期后节点不能重连 | 确认节点为 auto/system、系统时间和 CA 根证书正常、Nginx 加载了有效的完整证书链 |

全部节点确认使用 443 后，可以按部署需求关闭公网 9443，只保留 Nginx 到 Hub 的本机访问；不要关闭公网 SSH 端口池。
