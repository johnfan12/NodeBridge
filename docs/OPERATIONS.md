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

这会重新生成默认节点配置，本地 SSH 自定义端口需在新的 `config.json` 中恢复。旧 Linux 用户、SSH 服务和用户文件不受影响。

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

新版安装包执行同样的安装命令，会原子替换二进制并重启对应模式的服务，已有配置和数据库保留。首次管理员密码文件只存在于尚未修改初始密码的部署。

WebUI 随二进制一起更新。已安装用户重新执行 README 中对应模式的一键安装命令即可获取新版界面；升级后刷新浏览器。

联网安装使用 [最新 Release 安装脚本](https://github.com/johnfan12/NodeBridge/releases/latest/download/install.sh)，两种模式的命令见 README。默认下载最新版，可用 `NODEBRIDGE_RELEASE_BASE_URL` 固定版本或指定镜像。

卸载服务时：

```bash
sudo systemctl disable --now nodebridge-node
sudo rm /etc/systemd/system/nodebridge-node.service
sudo systemctl daemon-reload
```

hub 对应替换服务名。默认保留数据目录用于恢复；确认不再需要后再手工删除。若同一机器仍运行另一种模式，应保留共享二进制。
