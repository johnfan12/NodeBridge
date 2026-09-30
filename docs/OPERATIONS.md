# 运维

## 服务和日志

```bash
systemctl status nodebridge-hub
systemctl status nodebridge-node
journalctl -u nodebridge-hub -f
journalctl -u nodebridge-node -f
```

手动运行时输出日志到 stderr。hub 审计在管理员控制台“最近操作”展示。健康检查：

```bash
# 使用生成的证书直接信任本机 hub
curl --cacert /var/lib/nodebridge/hub/tls.pem https://你的VPS公网IP:9443/api/health

# 节点本地维护 API
curl http://127.0.0.1:9899/api/health
curl http://127.0.0.1:9899/api/node
```

## 节点连接不上

查看节点维护页面或 `journalctl -u nodebridge-node`。确认 VPS 的控制台端口可从节点访问、系统时间正确、TLS 文件未被替换。网络断开时无需重新配置，节点会自动重试。

“节点凭证已撤销”说明控制台删除过该节点，或恢复了不同时间的 hub 数据备份；需要重新配对。

配对链接已过期/已使用时重新生成。若先在 hub 创建了节点但节点未能保存兑换结果，先移除那个离线节点，释放它的公网端口。

## 没有桌面，如何粘贴链接

在节点终端执行 `nodebridge pair`，粘贴链接并回车。也可提供文件：

```bash
umask 077
cat > /tmp/nodebridge-pair.txt
# 粘贴链接，然后 Ctrl-D
nodebridge pair --file /tmp/nodebridge-pair.txt
rm /tmp/nodebridge-pair.txt
```

维护端口自定义时用 `nodebridge pair --listen 127.0.0.1:端口`。维护 UI 不允许绑定公网地址；需要通过 LAN SSH 做本地端口转发，或直接使用节点 CLI。

## 控制台显示在线，但 SSH 未就绪

```bash
ss -lnt | rg ':22\b'
ssh 你的Linux用户名@127.0.0.1
```

检查 sshd、Linux 用户、密钥和配置中的 `ssh_port`。控制台账号不代替 Linux 账号。没有 `nvidia-smi` 时只是缺少 GPU 指标，不会影响 SSH。

如果控制台显示 SSH 就绪但外网连接不上，检查 VPS 防火墙和云安全组是否开放分配的公网端口。“就绪”表示内部数据流和本地 SSH 已可用，不证明客户端到 VPS 的网络路径允许访问。

## 修改配置

停服后修改对应目录 `config.json`，再启动。所有配置均为 JSON，不需要同时编辑多个 .env、FRP INI 和节点列表。

```bash
sudo systemctl stop nodebridge-hub
sudoedit /var/lib/nodebridge/hub/config.json
sudo systemctl start nodebridge-hub
```

hub 已分配端口在数据库中保存。缩小端口池不会改变已有节点端口。修改 hub 公网地址或 TLS 证书需要重新配对节点；证书采用指纹锁定，不自动接受地址变更或新证书。

## 重新配对节点

先在 hub 管理界面移除旧节点。然后在节点执行：

```bash
sudo systemctl stop nodebridge-node
sudo rm /var/lib/nodebridge/node/config.json
sudo systemctl start nodebridge-node
nodebridge pair
```

这会重新生成默认节点配置，本地 SSH 自定义端口与 TCP 授权需在新配置中恢复；hub 中移除节点也会删除其全部 TCP 代理。旧 Linux 用户、SSH 服务和用户文件不受影响。

## 备份与恢复

停止 hub 后备份整个 `/var/lib/nodebridge/hub/`，包括 `hub.db`、`config.json`、`tls.pem` 和 `tls.key`。节点备份 `/var/lib/nodebridge/node/config.json`。数据目录包含认证凭证与私钥，需要保护备份访问权限。

```bash
sudo systemctl stop nodebridge-hub
sudo tar -czf nodebridge-hub-backup.tar.gz -C /var/lib/nodebridge hub
sudo systemctl start nodebridge-hub
```

恢复后确保数据归属 `nodebridge:nodebridge`、目录 `0700`、配置/私钥/数据库 `0600`。丢失 hub 证书会导致全部节点指纹验证失败；数据库和证书需一起恢复。

hub 重启时若原端口被其他服务占用，会保留端口记录并显示错误。停止占用该端口的服务，再重启 hub。不会偷偷把现有节点换到另一个端口。

## 更新与卸载

新版安装包执行同样的安装命令，会原子替换二进制并重启对应模式的服务，已有配置和数据库保留；服务重启会中断经过它的 SSH/TCP 连接，安排在空闲时更新。首次管理员密码文件只存在于尚未修改初始密码的部署。

WebUI 随二进制一起更新。已安装用户重新执行 README 中对应模式的一键安装命令即可获取新版界面；升级后刷新浏览器。

联网安装使用 [最新 Release 安装脚本](https://github.com/johnfan12/NodeBridge/releases/latest/download/install.sh)，两种模式的命令见 README。默认下载最新版，可用 `NODEBRIDGE_RELEASE_BASE_URL` 固定版本或指定镜像。

## 暂停转发与完整停服

需要保留 Web 时不要执行 `systemctl stop`。管理员在 VPS 的 `/admin` 管理页面“服务器”页签中展开“全局 SSH 转发设置”，使用全局暂停按钮；单节点暂停按钮在服务器一览和管理页的服务器行中；节点本机在 `http://127.0.0.1:9899` 的连接状态中暂停。无桌面节点的 curl 命令见 [README](../README.md#暂停--恢复-ssh-转发保留-web)。

暂停立即断开现有 SSH/传输，阻止新转发；管理连接、GPU 上报、配对、页面和原端口继续保留。VPS 全局、VPS 节点设置、节点本机三个开关独立，所有开关均恢复后才允许转发。hub 暂停存入 `hub.db`，node 暂停存入 `config.json` 的 `forwarding_paused`，重启/更新不会自动解除。在线历史继续采样，暂停期间 SSH 就绪统计为不可用。

需要连 Web 一起关闭时执行 `sudo systemctl stop nodebridge-hub` 或 `sudo systemctl stop nodebridge-node`；`start` 可恢复。要关闭开机启动，使用 `sudo systemctl disable --now nodebridge-node`（VPS 改成 `hub`）。

## 完整卸载

[README 的卸载命令](../README.md#卸载整个服务web--后端)下载 Release 的 `uninstall.sh`。脚本支持 `hub`、`node`、`all`；默认停止并禁用服务、移除 systemd 单元，保留数据。指定 `--purge` 才删除所选模式的数据目录，无法撤销。另一模式仍安装时保留共享二进制与系统账号；都卸载且数据都清除时移除共享程序和账号。

离线安装包：

```bash
sudo bash uninstall.sh node --purge
# VPS：sudo bash uninstall.sh hub --purge
# 本机两种模式：sudo bash uninstall.sh all --purge
```

卸载 node 后在 hub 控制台移除对应节点可撤销凭证、回收端口。卸载 hub 会中断所有 SSH/TCP 转发；清理数据后重装需重新创建账号并重新配对所有节点。卸载不会停止系统 sshd、修改防火墙或删除用户文件。手动部署的自定义进程、数据路径需自行清理。

## Overleaf / TCP 代理排障

先在节点执行 `curl http://127.0.0.1:8080`，确认服务正常；再在维护页授权实际端口。Docker/Toolkit 的宿主机端口需要绑定 `127.0.0.1` 或包含该地址，不能只监听容器内地址或另一块网卡。

VPS “代理”页签中：

- “端口未授权”：节点维护页未授权，或授权已经撤销。
- “服务未就绪”：授权存在但本机端口未监听，或者公网监听失败；列表会显示端口冲突详情。
- “节点离线”：检查节点服务和到 hub 的管理连接。
- “已暂停”：检查单代理开关以及节点本机 TCP 开关；SSH 开关不影响 TCP 服务。

状态正常但外网无法访问时，确认云安全组与 VPS 防火墙放行分配的端口。端口池由 SSH 和 TCP 共用。节点和服务状态每 10 秒上报，新增/撤销授权和暂停会触发及时上报；页面每 5 秒刷新。

原始 TCP 转发保留 WebSocket 和 HTTP 流量，服务自己的登录和权限负责访问控制。若 Overleaf 的重定向/链接仍指向 localhost，需要把 Overleaf 的站点地址改为外部地址，参见 [Overleaf 个性化配置](https://docs.overleaf.com/on-premises/installation/using-the-toolkit/5.-personalizing-your-instance)。NodeBridge 不修改 Overleaf 容器或站点配置。

暂停本机全部额外 TCP 转发（保留 SSH/Web）：

```bash
curl -fsS -X PUT http://127.0.0.1:9899/api/tcp-forwarding \
  -H 'Content-Type: application/json' -H 'X-NodeBridge-Request: 1' \
  -d '{"paused":true}'
# 恢复时改为 {"paused":false}
```

撤销某个本机端口授权：

```bash
curl -fsS -X DELETE http://127.0.0.1:9899/api/tcp-ports/8080 \
  -H 'Content-Type: application/json' -H 'X-NodeBridge-Request: 1'
```

TCP 代理重启时保留原端口；若该端口被其他进程占用，先释放它，再在代理列表点击“重试监听”（暂停的代理使用“恢复转发”）。删除代理释放公网端口，但不撤销节点的本机授权或停止实际服务。
