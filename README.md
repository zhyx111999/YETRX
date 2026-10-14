# YETRX

YETRX 基于 [mhtsec/ARTEX](https://github.com/mhtsec/ARTEX) 的 `a9d11cb` 原生源码维护，浏览器标题为 **YETRX-自主维护**，使用 Lucide 叶子图标。原项目说明保存在 [UPSTREAM_README.md](UPSTREAM_README.md)，许可证保留 AGPL-3.0；图标归属见 [web/THIRD_PARTY_NOTICES.md](web/THIRD_PARTY_NOTICES.md)。

## 原生 Agent 模式

Agent 是内置的一种普通对话模式，和其他模式共用 `ChatAgent`、模型配置、提示词管理和资源装配路径。没有内嵌 pi SDK、独立模型中继或 Node Agent 进程。

资源沿用上游管理方式：

| 资源 | 唯一管理位置 | 使用方式 |
| --- | --- | --- |
| 技能 | 全局 `skills/`（或配置的技能目录），系统 → 技能 | 按模式绑定；每轮读取当前文件 |
| 函数工具 | 原有数据库 `tools` 表，系统 → 函数工具 | 共用实现、描述和启用开关，按模式绑定 |
| MCP | 原有 MCP 配置，系统 → MCP | 按可见性加载，保留技能按需解锁 |
| SDK 基础工具 | 上游 `DefaultTools()` | 所有原生模式共用固定工具集 |
| 模型与模式 | 系统 → LLM / Agent | 继续沿用模型绑定、会话选择、全局默认以及可编辑提示词 |

共享资源仍保留上游的模式绑定：将同一个工具/技能绑定给多个模式，它们使用同一份定义。编辑、禁用或删除资源会在后续运行中统一生效。

首次将 Auto 原位改为 Agent 时保留 ID、对话、模型绑定和提示词历史，旧资源绑定引用一起迁移。已有 Agent 的自定义提示词、开关和绑定不会在重启时重置。Agent 的默认提示词只保留简洁工具说明，不复制个人身份提示或个人认证。

## 全局后台子任务

`subagents` 和 `subagents_control` 是原生全局函数工具，可在原有工具页面绑定给任意对话模式。每个子任务仍调用同一个 `ChatAgent`，从同一全局资源库读取配置。

```json
{
  "tasks": [
    {"label":"inspect","task":"检查指定源码并给出证据。"},
    {"label":"review","task":"复核 inspect 的结论。","depends_on":["inspect"]}
  ]
}
```

独立任务并行，有依赖的任务按顺序执行；前置任务失败时后续任务标记 blocked。启动立即返回 `run_id`，完成后结果自动回到根对话；当前对话正在运行时等待它结束后汇总。停止对话同时取消其后台任务和未执行的自动回复。

控制动作：`list`、`status`、`read`、`stop`、`resume`、`message`。`read` 一次读取一个 label，最多末尾 8000 字符。默认全局并发 4、每批最多 32 项；子代理硬性禁止再次派发或控制子代理。状态保存在数据目录的 `workflows/`。

| 机制 | 当前行为 |
| --- | --- |
| 运行中指令 | `message` 向正在运行的任务排队，在下次模型调用前进入同一子会话；若先结束则丢弃，不打断正在执行的工具 |
| 恢复与编辑 | `resume` 可携带编辑后的 tasks；未变成功的一次性任务重排后仍复用，变更及其下游重跑，旧结果保留历史 |
| 检查点 | checkpoint 步骤持久化等待；通过匹配的 checkpoint_id/response 继续，支持 JSON null；服务重启不丢等待状态 |
| 命名会话 | thread 在一次运行内共享历史和固定 cwd；失败回合回滚，工作流恢复时重新执行 thread 回合 |
| 自动重试 | agent_retries 默认 0、最多额外 3 次；只重试可恢复错误，必须等旧执行器退出；每次使用新的一次性会话 |
| 额度恢复 | 提供商额度限制单独暂停，按 reset 提示和有上限的退避自动恢复，最多 5 次；手动 stop 取消定时器 |
| 源码目录 | 默认共享运行 cwd；显式 isolation:"worktree" 创建独立 Git 分支和源码树，默认保留，不自动合并，创建失败不降级 |

同次任务自动重试共享其工作树；恢复后重新执行的任务创建新工作树。`keep_worktree:false` 在执行器退出后删除该任务的工作树和分支。独立子会话不复制父完整历史。服务重启将仍运行的任务标为 interrupted，普通中断需手动恢复；resume 不恢复 Go 调用栈，也不保证工具副作用只执行一次。

五项机制参照当前 `zhyx111999/pi-workflows-lean` 源码，以原生 Go/ChatAgent 实现；模型仍继承父会话，全局并发仍为 4，流程使用原生 DAG。详细契约和示例见 [workflow-authoring](skills/workflow-authoring/SKILL.md)，第三方许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

三个说明技能 `workflow-authoring`、`workflow-patterns`、`subagents-setup` 位于统一 `skills/` 中，已按原生工具重写。流程使用任务依赖，不执行旧 pi 的 JavaScript 工作流脚本。没有安装 Tavily。

## 会话发文件

Agent 可调用全局 `send_file` 工具，将已生成的文件直接发到当前聊天。消息中显示文件名、大小和下载按钮；下载沿用现有登录认证。默认绑定 Agent 模式，可在函数工具页面调整模式绑定。

```json
{"path":"reports/result.pdf","name":"分析报告.pdf","caption":"报告已完成，可在这里下载。"}
```

相对路径按当前会话工作目录解析，也可发送绝对路径。支持普通文件，单个最多 128 MiB；目录先打包。服务端保存独立副本，源文件修改或工作树删除不会改变已发附件；删除会话会清理该会话的附件。子任务将产物路径交给主代理，由主代理审查后发送。

## 构建与运行

需要 Go 1.26 和用于前端构建的 Node.js；服务端运行不需要 pi 或 Node Agent 运行时。保留 `artex` 二进制名和 `ARTEX_*` 环境变量，以兼容已有部署。

```bash
./build.sh --target linux/amd64
# 部署生成的 artex、skills/ 以及现有配置和数据。
./artex -addr :18787
```

数据库、模型密钥、JWT 签名密钥、历史和用户编辑的技能属于现有实例，应保留，不能用源码包中的默认文件覆盖。工具以服务用户身份运行；对话与子任务工作目录用于组织文件，不构成操作系统隔离。

旧内嵌版本的源码保存在 `archive/pi-integration` 分支。使用过旧运行时的对话，在首次原生运行前从活动记录重建上下文，旧工具调用作为历史文字保留，避免重放。原始文件和旧原生 transcript 副本保留供回溯；网页记录不删除。

版本检查指向 `zhyx111999/YETRX`。原生二进制更新路径保留；全局技能仍由技能管理维护。Docker 发布只有显式配置本仓库的 `PUBLISH_DOCKER` 与 `DOCKERHUB_IMAGE` 时启用，默认使用原生部署。

验证范围及已知上游限制见 [VALIDATION.md](VALIDATION.md)。
