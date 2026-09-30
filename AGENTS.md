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
