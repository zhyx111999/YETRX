# YETRX · 自主维护

YETRX 是基于 [mhtsec/ARTEX](https://github.com/mhtsec/ARTEX) 维护的分支。网页默认对话模式为 **Agent**，通过真正的 pi SDK 执行文件、命令和后台工作流。浏览器标题为 **YETRX-自主维护**，标识采用 Lucide 的叶子图标。

## Agent

- 工具：`read`、`bash`、`edit`、`write`、`grep`、`find`、`ls`、`subagents`、`subagents_control`。
- 技能：`workflow-authoring`、`workflow-patterns`、`subagents-setup`，按需读取其 `SKILL.md`。
- pi SDK 固定为 `@earendil-works/pi-coding-agent@1.1.0`；工作流扩展使用仓库内保留源码和许可的 `pi-workflows-lean`。
- 不安装 Tavily。工作流包自带的研究功能仍可使用其网页搜索和抓取工具。
- 默认系统提示只有简洁的工具说明；网页保存的 Agent 提示词在后续对话中生效。不会复制操作者本机的身份提示、`APPEND_SYSTEM.md`、账户或 API 密钥。
- 每个对话使用持续存活的 pi 会话，后台子代理在主轮次完成后仍能返回结果。会话文件支持进程重启后续接。

模型从网页的 LLM 配置中选择。Go 的本机中继保留现有模型适配器、代理、请求记录和回退机制；上游 API 密钥留在 Go 端，pi 仅持有临时中继令牌。子代理可选择 `yetrx/global` 或 `yetrx/profile-<配置ID>`。模型配置的思考强度由 Go provider 执行。

Agent 使用 pi 自己的工具和技能。原平台的自定义工具、MCP、数据库技能绑定仍供其他原生模式使用；这些旧绑定不会自动注入 Agent。终端专用 TUI 面板不在网页渲染，工作流状态和结果通过工具记录及后台回传提供。

## 从源码运行

需要 Go 1.26.3、Node.js 24 LTS、npm 和 PostgreSQL。Linux 推荐安装 `ripgrep` 与 `fd`（Ubuntu 的软件包名为 `fd-find`，可执行文件名为 `fdfind`）。

```sh
cd web
npm ci
npm run build:static
cd ..
mkdir -p server/webui/dist
cp -a web/out/. server/webui/dist/

npm --prefix runtime/pi ci --omit=dev
go build -tags embedui -o artex ./cmd/artex

export ARTEX_PG_DSN='postgres://USER:PASSWORD@127.0.0.1:5432/DATABASE?sslmode=disable'
./artex -addr :18787 -proxy :8788
```

`runtime/pi/` 必须与可执行文件一起部署。`YETRX_PI_NODE` 可指定 Node 的绝对路径，`YETRX_PI_BRIDGE` 可指定运行时入口。Node 24 下的网页请求继承进程代理环境；模型请求仍由 Go provider 按网页配置和环境代理执行。

对话工作目录位于 `data/sessions/conv-<ID>/`，pi 状态位于 `data/pi/`。这些目录只用于保存工作与会话状态，工具仍以服务用户身份访问宿主环境。工作流记录由扩展保存在服务用户的 `~/.pi/workflows/`。

## 既有安装

启动迁移会将内置 Auto 原位改为 Agent，保留原 ID、模型绑定、对话、活动和所有提示词历史，并追加新的最小默认提示词。旧领域工具及技能不会自动迁入新运行时。已不存在或被禁用的 Agent 不再被旧触发器调度。

为兼容既有安装，Go module 路径、`artex` 可执行文件名、`ARTEX_*` 环境变量和认证存储键继续使用原名。登录接受 `YETRX` 与旧用户名 `ARTEX`，旧签名令牌继续有效。

更新时需同步部署 Go 可执行文件与 `runtime/pi/`，并按 lockfile 安装运行时依赖；保留既有 `data/`、`jwt.key`、数据库和配置。内置版本检查只访问 [zhyx111999/YETRX](https://github.com/zhyx111999/YETRX)。上游的二进制更新器尚未实现 Node 运行时整包换装，因此目前应按源码部署流程更新完整安装。

## 验证

```sh
go test ./pibridge ./server ./db ./agent ./selfupdate
npm --prefix runtime/pi test
```

Go/pi 联调测试使用实际 pi SDK、本地假模型与真实文件工具，不需要上游 API 密钥。数据库迁移测试使用显式指定的独立测试数据库；不要将应用的生产 DSN 用作测试库。具体测试变量见 `db/agent_mode_test.go`。

## 来源与许可

应用保留 AGPL-3.0 许可，原项目说明见 [UPSTREAM_README.md](UPSTREAM_README.md)。第三方工作流代码保留 MIT 许可与来源记录；叶子图标的 ISC 许可见 [web/THIRD_PARTY_NOTICES.md](web/THIRD_PARTY_NOTICES.md)。
