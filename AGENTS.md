# NodeBridge

- 本仓库是 Go 单仓库重构，发布一个二进制，用 `hub` / `node` 两种模式部署。
- 正式 Web 控制台属于 hub；node 页面仅用于配对与诊断。
- 当前功能基线是旧仓库 `simple-tunnel-platform`，完整 Docker 实例与计费不属于已经交付的范围。
- 安装体验优先：生产运行不依赖 Python、npm、FRP 或外部数据库。
- 正式仓库为 https://github.com/johnfan12/NodeBridge，安装器默认使用该仓库最新 Release；支持 NODEBRIDGE_RELEASE_BASE_URL 覆盖镜像或版本地址，文档中的下载链接必须可用。
- 节点仅允许转发本机回环 SSH 端口。增加其他执行能力前必须明确鉴权、允许列表和权限。
- 数据更新要有事务或原子替换；邀请兑换必须单次消费，失败端口分配不能消费邀请。
- 管理员预创建，每节点独立凭证，禁止首个公开注册者成为管理员。
- 核心配对/转发/鉴权变动运行 `make check`，发布运行 `make release VERSION=<version>`。
- 功能变化同步更新 README、docs/MIGRATION.md 和运维说明。

- 从 v0.3.0 起 NodeBridge 只负责 SSH；额外 TCP/Overleaf 转发在独立 PortBridge 仓库维护。旧 TCP 配置与数据库记录仅为迁移保留，不注册接口、不监听、不转发。

- v0.5.0 增加受限升级控制类型 4、发布下载中转类型 5，类型 3 继续拒绝。升级 API 仅管理员可调用；节点不接受远程 shell、下载 URL、二进制路径或期望校验和。
- Web/节点进程保持普通用户；独立 systemd.path + root oneshot 执行器只处理固定目录的版本请求，从固定官方仓库通过正常 HTTPS 独立取得 SHA256SUMS 并校验发布包，再原子替换共享二进制。node 下载经 Hub 传输 TLS 字节，root 执行器仍独立校验 GitHub 域名/证书，不能信任节点进程提供的校验和。
- 更新要覆盖角色权限、旧节点兼容、重复请求、下载失败不重启、校验失败不安装、启动失败恢复旧程序及下载目标允许列表。首次启用面板升级需运行新版安装脚本安装执行器。
