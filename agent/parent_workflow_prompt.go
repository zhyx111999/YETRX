package agent

import "context"

// ParentWorkflowPrompt is the native equivalent of pi-workflows-lean's hidden
// parent prompt. It is added at the request boundary for a human turn, so the
// persisted user activity and transcript keep the user's original message.
// Native YETRX workflows use tasks/depends_on and subagents_control; the pi
// JavaScript workflow runtime is intentionally not part of this path.
const ParentWorkflowPrompt = `[子代理模式已启用。平台区分两类委派：子任务（探索）通过 spawn_task 等探索任务工具建立，围绕探索目标由规划者持续规划、执行器领取探索意图并写回结果；子代理（对话）通过 subagents 启动独立会话，完成一次明确的求证、审查或小范围修改后返回，由主对话核验和交付。子任务使用 task_id/intent_id，子代理使用 run_id/label；两套入口、状态和并发设置分别管理。
先判断是否需要独立求证或可并行的小范围工作。闲聊、一两步就能完成的操作直接做；持续推进的探索目标使用探索子任务；有明确输入、范围、预算和交付标准的独立事项才派发子代理。用 tasks 列出每个子代理的工作说明；有先后步骤时用 depends_on 连接，使用平台原生 subagents 和 subagents_control，不执行 pi 的 JavaScript agent()/parallel()/pipeline()。如用户要求你直接完成、不使用子代理，遵守该要求。一个子代理只负责一件范围明确的事，不自行扩展为持续探索目标。需改写 Git 项目时，在 cwd 指定源码根目录，并为该项设置 isolation:"worktree"；默认保留工作树，结果不自动合并，由你审查后集成。你负责方案、最终执行与审查，始终是主架构员。
给子代理的提示词里要求它把这次实际执行的操作、依据和结果写进最终返回。结果回来后，你对照记录核验，决定是否采纳、合并或继续查证。不要把原始长记录原样复述给用户，除非用户要求。
subagents_control 的 message 可向正在运行的 label 追加指令，在其下一次模型调用前送达，提前结束则丢弃。resume 可以携带编辑后的 tasks，复用未变成功步骤，变更及其下游重跑；等待 checkpoint 时用 checkpoint_id 和 response 回答。子代理不可再次调用编排工具。子代理在后台运行；同一父对话的多批运行共用该对话的并发额度，不同父对话互不占用。子代理返回时若主对话正在输出，结果先排队，当前整轮输出及工具执行结束后立即依序送入，不打断本轮，也不需要用户再发消息。用户要求查看时，一次只用 read 读取一个子代理的末尾最多 8000 字符；明确提供 run_id 和 label，先用 list/status 确认目标，不假定空参数会自动选择最近运行。]`

type parentWorkflowPromptKey struct{}

// WithParentWorkflowPrompt marks a top-level human turn for native workflow
// guidance. Child workflow sessions do not inherit this marker, which prevents
// a delegated task from recursively receiving the parent-only instruction.
func WithParentWorkflowPrompt(ctx context.Context) context.Context {
	return context.WithValue(ctx, parentWorkflowPromptKey{}, true)
}

// HasParentWorkflowPrompt reports whether the current turn is a top-level human
// message that should receive the parent workflow guidance.
func HasParentWorkflowPrompt(ctx context.Context) bool {
	marked, _ := ctx.Value(parentWorkflowPromptKey{}).(bool)
	return marked
}
