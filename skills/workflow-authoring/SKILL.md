---
name: workflow-authoring
description: 设计原生后台子任务的并行分工、依赖顺序和结果审查；使用全局 subagents 工具。
---

# 原生工作流

所有任务使用平台已有的对话模式、模型配置、全局工具、技能和 MCP。通过 `subagents` 的 `tasks` 数组描述流程：

```json
{
  "tasks": [
    {"label": "inspect", "task": "检查指定源码，说明问题和证据。只读。"},
    {"label": "design", "task": "根据 inspect 的结果给出最小修改方案。", "depends_on": ["inspect"]},
    {"label": "review", "task": "独立复核设计、风险及验证步骤。", "depends_on": ["design"]}
  ]
}
```

- 无依赖的任务并行执行；`depends_on` 指向同批次的 label。依赖全部成功后才启动，结果会作为上下文交给后续任务。
- label 在本批次内唯一。每批最多 32 项，依赖不能成环。默认同时执行 4 个子任务，所有模式共用并发额度。
- `agent_key` 可选择全局模式，省略则沿用当前模式。子任务自动使用启动它的当前对话的 ChatAgent，因此继承同一模型配置解析结果、全局工具、技能和 MCP；任务不接收单独的 LLM 配置。
- 子任务有独立会话和工作目录，仍以平台服务用户访问宿主机。涉及源码修改时先在独立副本完成，提交实际修改和验证结果，主对话审查后再集成。
- 完整交代输入位置、任务边界和预期结果。最终报告应写清实际操作、理由、证据、失败和未完成项。
- 启动立即返回 `run_id`；后台结束会自动回传根对话，无须反复查询。
- 原生流程使用上述任务依赖，不执行 pi 的 JavaScript `agent()/parallel()/pipeline()` 脚本。

用 `subagents_control` 的 `status` 查看状态、`read` 读取一个子任务结果、`stop` 停止、`resume` 重试未完成部分。恢复会保留成功项；重试前先检查部分执行产生的改动。
