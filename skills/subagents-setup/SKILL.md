---
name: subagents-setup
description: 使用平台现有的全局模式、模型、技能、函数工具和 MCP 管理配置原生子任务。
---

# 原生子任务配置

资源只在平台现有位置维护，没有单独的 Agent 或子任务技能目录。

- **系统 → Agent**：管理对话模式、提示词、启用状态、轮次和时间预算。子任务通过 `agent_key` 选择同一份模式配置。
- **系统 → LLM 与人格**：管理全局模型配置。`subagents` 自动沿用启动它的当前对话的 ChatAgent 和模型配置，不提供子任务级 `llm_profile_id` 覆盖；当前对话的 Agent 绑定、会话选择和全局激活配置按现有解析链继承。
- **系统 → 函数工具**：管理共享工具的描述、启用状态和模式绑定。将 `subagents`、`subagents_control` 绑定给需要它们的模式。
- **系统 → 技能**：编辑同一个全局技能目录并设置模式可见性。技能内容和工具描述的修改在下次原生对话/子任务运行时读取。
- **系统 → MCP**：管理共享服务及模式可见性。技能引用的 MCP 继续遵守平台原有的按需解锁机制。

SDK 基础工具（Bash、Read、Write、Edit、MultiEdit、LS、Glob、Grep）按上游方式由所有原生模式共用；它们不是每个模式各存一份的资源。

工具调用示例：

```json
{"tasks":[{"label":"review","agent_key":"agent","task":"独立检查指定文件并报告证据。"}]}
```

启动后保留返回的 `run_id`。控制工具支持 `list`、`status`、`read`、`stop`、`resume`、`message`，例如：

```json
{"action":"read","run_id":"返回的ID","label":"review","max_chars":8000}
```

结果保存于平台数据目录的 `workflows/`；服务重启将未完成运行标为 interrupted，手动 resume 才重新调度。编辑恢复时复用未变且成功的一次性任务，变更项及其下游重跑；命名 thread 回合不会复用。checkpoint 的等待与响应持久化。

`subagents` 可设置运行 `cwd`、`agent_retries`（默认 0、最多额外 3 次）和 `agent_timeout_ms`（默认 0）；任务可选 `isolation:"worktree"`、`keep_worktree`、`thread`。默认共享源码目录，显式 worktree 默认保留且不自动合并。只有可恢复错误在旧执行器退出后才重试。额度限制单独暂停并有限自动恢复，stop 会取消定时器。详细语义和示例见 workflow-authoring。

子代理始终不能调用 subagents/subagents_control；即使模式绑定了这两个工具，也会在子会话中被排除。现有全局并发仍为 4，与探索引擎的 Work Agent 数独立。

无需安装 pi、Node Agent 运行时、个人插件或复制个人认证文件。
