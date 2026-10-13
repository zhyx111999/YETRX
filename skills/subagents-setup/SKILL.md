---
name: subagents-setup
description: 使用平台现有的全局模式、模型、技能、函数工具和 MCP 管理配置原生子任务。
---

# 原生子任务配置

资源只在平台现有位置维护，没有单独的 Agent 或子任务技能目录。

- **系统 → Agent**：管理对话模式、提示词、启用状态、轮次和时间预算。子任务通过 `agent_key` 选择同一份模式配置。
- **系统 → LLM**：管理全局模型配置。`subagents` 自动沿用启动它的当前对话的 ChatAgent 和模型配置，不提供子任务级 `llm_profile_id` 覆盖；当前对话的 Agent 绑定、会话选择和全局激活配置按现有解析链继承。
- **系统 → 函数工具**：管理共享工具的描述、启用状态和模式绑定。将 `subagents`、`subagents_control` 绑定给需要它们的模式。
- **系统 → 技能**：编辑同一个全局技能目录并设置模式可见性。技能内容和工具描述的修改在下次原生对话/子任务运行时读取。
- **系统 → MCP**：管理共享服务及模式可见性。技能引用的 MCP 继续遵守平台原有的按需解锁机制。

SDK 基础工具（Bash、Read、Write、Edit、MultiEdit、LS、Glob、Grep）按上游方式由所有原生模式共用；它们不是每个模式各存一份的资源。

工具调用示例：

```json
{"tasks":[{"label":"review","agent_key":"agent","task":"独立检查指定文件并报告证据。"}]}
```

启动后保留返回的 `run_id`。控制工具支持 `list`、`status`、`read`、`stop`、`resume`，例如：

```json
{"action":"read","run_id":"返回的ID","label":"review","max_chars":8000}
```

结果保存于平台数据目录的 `workflows/`；这是运行记录，不是另一套资源管理。服务重启会把未完成任务标为 interrupted，由用户决定是否 resume，避免自动重复有副作用的操作。成功任务不会在恢复时重新执行。

无需安装 pi、Node Agent 运行时、个人插件或复制个人认证文件。
