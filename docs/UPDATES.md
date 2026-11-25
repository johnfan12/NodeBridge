# 中心版本管理与面板升级

## 使用流程

管理员打开 **管理页面 → 版本更新**：

1. 查看控制台 Hub、在线节点和离线节点最后上报的版本、架构。
2. 点击“检查更新”，读取 `johnfan12/NodeBridge` 最新正式 Release。检查只获取版本信息，不安装，也不会在后台自动升级。
3. 对需要更新的 Hub 或在线节点点击“升级”，确认目标版本和连接中断提示。
4. 页面自动展示等待、检查、下载、校验、安装、重启及完成/失败状态。失败原因保留，可在排除原因后重试。

只支持升级到比当前版本更高的 `vX.Y.Z` 正式版本格式。普通用户不能检查更新或提交升级任务。旧节点没有版本字段，显示“未上报版本”；离线节点不可下发升级。

## 首次启用：两端安装一次新版

v0.4.1 及更旧程序不支持升级指令。Hub 和每台 node 必须先安装 v0.5.0 或更新版本，由 root 安装脚本配置独立更新执行器；之后的新版本可在面板完成升级。没有执行器的手动运行部署只展示版本，按钮禁用。

普通升级自动沿用已有地址、配对与端口，无需 URL 或 `--reconfigure` 参数。VPS 执行：

```bash
curl -fsSL https://github.com/johnfan12/NodeBridge/releases/latest/download/install.sh \
  | sudo bash -s -- hub
```

每台已配对节点使用后台任务更新：

```bash
sudo systemd-run --unit=nodebridge-node-bootstrap --collect /bin/bash -c '
set -euo pipefail
curl --connect-timeout 15 --max-time 180 --retry 3 -fsSL https://github.com/johnfan12/NodeBridge/releases/latest/download/install.sh |
  bash -s -- node
'
```

需要更换域名或从临时的 `域名:9443` 迁移到域名 443 时，另按[地址迁移说明](NGINX.md#将临时的-9443-入口切回域名-443)修改连接地址。尚未配对的节点普通安装 node 后，用新版 Hub 新生成的链接配对。初次升级仍需安装机能下载 Release；网络不稳定时，可以在可联网机器下载对应架构包及 SHA256SUMS，校验后通过已有 SSH 传到节点，再离线运行：

```bash
# 已校验并解压安装包后执行；后台任务独立于当前 SSH 会话。
sudo systemd-run --unit=nodebridge-node-bootstrap-local --collect \
  /bin/bash /绝对路径/install.sh node --binary /绝对路径/nodebridge
```

需要迁移连接地址时在上面的命令追加 `--hub-url https://你的域名 --tls-mode auto --reconfigure`。

## 升级时的影响

- 检查、下载和 SHA-256 校验期间，当前服务和 SSH 转发继续运行。
- 安装重启 node 会断开该节点的 SSH 会话；安装重启 Hub 会断开经它转发的全部 SSH 会话。
- hub/node 在同一台机器共享一个二进制。执行器使用全局文件锁串行安装，并重启本机已经运行的另一种模式，让两种进程使用同一版本。
- 已有账号、配对凭证、固定公网端口、暂停状态和 Nginx/Certbot 配置保留。此功能不变更域名、证书或防火墙。
- 下载失败或校验失败不替换当前程序。新版未在 30 秒内通过版本健康检查时，恢复旧二进制并重新启动原服务，展示恢复结果。此机制不替代运维数据备份；未来涉及不兼容数据迁移的版本仍需遵循该版本迁移说明。
- 更新执行器独立于 Web、node 和 SSH 会话运行，服务重启或浏览器关闭不会取消已经提交的任务。

## 节点下载与信任边界

node 更新时，root 执行器通过回环维护页打开字节连接，沿节点现有 TLS/WebSocket/yamux 隧道由 Hub 连接 GitHub。节点无需直接访问 GitHub，不需要新增公网端口。

root 执行器自己与 GitHub 建立正常验证的 HTTPS，分别取得固定仓库的 SHA256SUMS 和架构包。节点及 Hub 中转的是 TLS 字节，不能替换校验和或安装包。下载目标限定为 GitHub 官方 API、仓库与发布文件域名的 443 端口，拒绝其他地址及不受信任的重定向。

控制请求只包含任务 ID 和版本；不接受命令、URL、路径或调用者提供的校验和。不执行下载包中的安装脚本，仅提取并校验 `nodebridge` 二进制。执行器拒绝旧版本、无效版本、请求文件符号链接和未知字段。安装期间不关闭节点指纹校验或 GitHub 证书校验。

## 文件与服务

| 项目 | 路径或名称 |
| --- | --- |
| 等待处理的请求 | `/var/lib/nodebridge/<hub或node>/update-request.json`，普通服务用户写入 |
| root 执行器状态 | `/var/lib/nodebridge-updater/<hub或node>/status.json`，root 写入，服务只读 |
| 旧二进制 | `/var/lib/nodebridge-updater/<hub或node>/private/previous-nodebridge`，root 私有目录 |
| 文件监听服务 | `nodebridge-update-hub.path` / `nodebridge-update-node.path` |
| 更新执行服务 | `nodebridge-update-hub.service` / `nodebridge-update-node.service` |

```bash
# VPS：查看升级执行日志
sudo journalctl -u nodebridge-update-hub -n 100 --no-pager

# 节点：查看升级执行日志和运行状态
sudo journalctl -u nodebridge-update-node -n 100 --no-pager
sudo systemctl status nodebridge-node --no-pager
curl -fsS http://127.0.0.1:9899/api/node
```

如果路径监听器未启用或多次失败触发 systemd 限制，排除日志中的原因后恢复：

```bash
# 节点示例，VPS 将 node 替换为 hub。
sudo systemctl reset-failed nodebridge-update-node.service nodebridge-update-node.path
sudo systemctl enable --now nodebridge-update-node.path
```

卸载脚本会一并停用对应更新执行器。`--purge` 同时删除其状态与旧二进制备份；保留另一模式的执行器与数据。
