---
name: workflow-authoring
description: 用 subagents 为独立求证、审查或小范围修改编排子代理，支持依赖、运行中指令、恢复与源码工作树；不是探索子任务。
---

# 子代理工作流

子代理通过 `subagents` 启动独立会话，一次负责一件输入、范围和交付标准明确的小事，适合求证、审查和小范围修改；返回实际操作、依据和结果，由主对话核验后采纳或合并。它不是探索子任务：后者由 `spawn_task` 等探索入口启动，围绕探索目标持续规划、领取探索意图并执行。

子代理使用平台已有的 ChatAgent、模型解析链、全局工具、技能和 MCP。用户要求直接完成时不要派发子代理。流程用 tasks/depends_on 表达，tasks 是兼容的请求字段，不代表探索子任务；不执行 pi JavaScript 脚本。

```json
{
  "cwd": "/path/to/project",
  "agent_retries": 1,
  "agent_timeout_ms": 120000,
  "tasks": [
    {"label":"inspect","task":"只读检查指定文件，输入、范围及预期结果必须写全。"},
    {"label":"implement","task":"根据检查结果实现约定修改；报告分支、文件、提交、命令及测试结果。","depends_on":["inspect"],"isolation":"worktree"}
  ]
}
```

- label 唯一，每批最多 32 项，依赖不能成环。独立项并行，依赖全部成功后启动；依赖结果显式交给后续任务。每个父对话独享 4 个子代理执行槽，该对话的多批运行合计不超过 4；不同对话互不占用，与系统配置中的「子任务执行并发（探索）」独立。取消或超时后尚未真正退出的执行器仍占用自己会话的槽位，不影响其他会话。
- agent_key 只选择提示词和资源可见性。模型始终继承父对话当前实际 ChatAgent，没有子代理级模型覆盖。
- 默认共享运行 cwd，省略运行 cwd 时使用父会话目录；任务 cwd 可选择其他已存在目录。不同一次性任务/重试使用独立子会话，不自动继承父历史。
- isolation:"worktree" 从运行 cwd 所在 Git 仓库 HEAD 创建独立分支和工作树。不会复制父目录未提交修改，不自动合并；默认保留供主代理审查，keep_worktree:false 在执行器退出后移除工作树和分支。创建失败终止任务，不退回共享目录。任务 cwd、thread 均不能与 isolation 同时使用。
- thread 用于同次运行内连续对话；相同 thread 的任务必须通过 depends_on 串行，并保持相同 canonical cwd。失败回合恢复前一 transcript，失败后缀另存审计。恢复整个工作流时，thread 任务重新执行，旧成功 thread 回合不作为可复用值。
- agent_retries 默认 0，上限 3，表示额外次数。只重试可恢复错误，退避 250/500/1000ms；必须等待旧执行器退出。取消、配置错误、checkpoint 和额度暂停不作为普通重试。
- agent_timeout_ms 默认 0，不额外设置硬超时；模式原有轮次和时间预算仍适用。超时/取消后给执行器 10 秒退出宽限；仍未退出时标记 draining、禁止重试和恢复，保留槽及工作树直到真正退出。
- 子会话硬排除 subagents、subagents_control、spawn_task，包括延迟工具调用入口；服务端再次拒绝嵌套派发。需要追加工作时交回主对话，探索内部 Planner → Worker 调度不受影响。

# 控制和恢复

主聊天的停止只中断当前回复，子代理继续执行且完成后仍会回传。要停止指定运行，用 subagents_control stop；删除会话则停止其子代理并清理回传。通过主对话 spawn_task 创建的探索任务也会回传首次结果，使用 pause_task 或任务页面单独管理。

启动立即返回 run_id；结束或暂停会自动回传。主对话忙碌时，子代理执行记录和结果先排队：文字生成中不插入；当前这次响应结束后立即依序显示，同一轮后续的模型请求在工具结果之后读取，正在执行的工具不被取消或重跑；若没有后续请求，则本轮结束后自动跟进。无须用户再发消息。status 查看状态、尝试记录、源码目录、分支和检查点；read 明确提供 run_id 和 label，一次读取一个子代理，最多末尾 8000 字符。

```json
{"action":"message","run_id":"wf-...","label":"implement","text":"新增范围约束：只修改指定模块，并报告未验证项。"}
```

message 只向实际运行的子代理排队，在下一次模型调用前写入其会话；当前模型请求与工具不被打断。子代理先结束则丢弃尚未送达的消息，不在重试时重放。

```json
{"action":"resume","run_id":"wf-...","tasks":[{"label":"inspect","task":"保持原任务原文。"},{"label":"implement","task":"这里填写修订后的完整任务。","depends_on":["inspect"],"isolation":"worktree"}]}
```

resume 接受 failed、stopped、interrupted、paused；携带 tasks 时也可编辑已完成运行。未变且成功的一次性任务即使重排也会复用；变更、删除及其依赖结果失效，旧结果留在历史。失败任务重新执行，不能假设恢复到某条工具调用的精确断点。每次新的 worktree 执行创建新路径；同一次执行的自动重试共享原工作树。

显式检查点用一个只含 checkpoint 的步骤表示：

```json
{"tasks":[{"label":"review-gate","checkpoint":{"checkpoint_id":"review-1","kind":"review","payload":{"summary":"待审查修改"}}},{"label":"continue","task":"依据 review-gate 的响应继续。","depends_on":["review-gate"]}]}
```

运行持久化并暂停；用 `{"action":"resume","run_id":"wf-...","checkpoint_id":"review-1","response":{"approved":true}}` 回答。response 可以是任意 JSON，包括 null；必须匹配当前 ID。等待回答时不能同时编辑 tasks。服务重启仍保留等待项和已经完成的结果。

提供商额度/速率限制单独暂停，按 reset 提示、指数退避与 ±10% 抖动自动恢复；默认约 5 分钟，最少 1 分钟、最多 6 小时，每轮最多 5 次。次数和计划时间可在 status 查看；冷启动续用计数。手动 resume 重置这一轮计数，stop 取消定时器。普通 interrupted 不自动执行。
