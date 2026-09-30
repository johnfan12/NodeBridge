# 首版验证记录

日期：2026-09-30。环境：Linux amd64，Go 1.27.1。

## 已执行

| 验证 | 结果 |
| --- | --- |
| Go vet | 通过 |
| `go test -race ./...` | 通过 |
| 真实 HTTPS hub 与节点连接 | 通过 |
| 大于 yamux 窗口的数据传输、TCP 半关闭 | 通过，完整回传 800,000 字节测试数据 |
| 节点重连、hub 重启恢复同一端口 | 通过 |
| 并发使用同一个邀请 | 仅一个成功 |
| 端口占用跳过、耗尽失败回滚、过期邀请拒绝 | 通过 |
| 删除节点关闭监听、凭证撤销 | 通过 |
| 普通用户权限、会话撤销、管理员保护 | 通过 |
| TLS 指纹错误、跨站写请求、节点 Host 校验 | 拒绝符合预期 |
| 配置文件中公开节点维护监听地址 | 启动校验拒绝 |
| 存储事务回滚、重开持久化、单写者锁 | 通过 |
| 嵌入页面和静态资源实际 HTTP 访问 | 通过，修复并覆盖首页重定向问题 |
| JS 与 Bash 语法检查 | 通过 |
| Playwright 实际浏览器 | 登录、UI 配对、SSH 命令、创建账号、修改密码、普通用户界面均通过 |
| 浏览器手机宽度 390 px | 无横向溢出 |
| Linux amd64/arm64 Release 构建 | 通过，二进制与安装脚本已打包 |
| systemd 服务文件语法 | 通过，使用临时文件验证 |

浏览器验收使用临时开发工具，没有给仓库或生产二进制增加 Playwright/npm 依赖。截图保存在本机 `dist/ui-hub.png`、`dist/ui-node.png`、`dist/ui-mobile.png`，属于不入 Git 的构建产物。

本机自带 Go 1.18.1 没有更改；验证所用的新工具链下载并校验到 `/tmp/nodebridge-go/go/`。因此在本次工作环境直接重新构建时可用：

```bash
make check build release GO=/tmp/nodebridge-go/go/bin/go VERSION=v0.1.0
```

## 尚需部署验收

- 此记录描述首次本地构建；后续发布状态以 [GitHub Releases](https://github.com/johnfan12/NodeBridge/releases) 和 [Actions](https://github.com/johnfan12/NodeBridge/actions) 为准。
- 真实 VPS 到内网节点的外网连通、云安全组、SSH 登录/文件传输、长期断网恢复，需在目标机器上验收。
- 安装脚本和 systemd 文件已检查，未实际修改本机系统服务或安装专用系统账号。
- arm64 已交叉构建，尚未在真实 arm64 机器运行。
- 旧用户数据库没有导入。可选 PAM 登录、SSH 命令书签、历史 Docker/配额/计费功能仍见迁移清单。
