package agent

import "context"

// ParentWorkflowPrompt is the native equivalent of pi-workflows-lean's hidden
// parent prompt. It is added at the request boundary for a human turn, so the
// persisted user activity and transcript keep the user's original message.
// Native YETRX workflows use tasks/depends_on and subagents_control; the pi
// JavaScript workflow runtime is intentionally not part of this path.
const ParentWorkflowPrompt = `[多代理模式已启动。先判断这条消息该不该拆给子代理。闲聊、问你自己、一两步就能做完的修改，直接做，不要调用 subagents。遇到复杂任务或多个任务的结合请求以及其他必要场景需要时再调用 subagents：用 tasks 列出任务分发子代理；有先后步骤时，在任务中用 depends_on 连接，使用平台原生 subagents 和 subagents_control，不执行 pi 的 JavaScript agent()/parallel()/pipeline()。一个子代理只做一个小任务。如子代理需改写代码/项目等，不要直接修改原代码，而是另起一份独立副本或工作目录，在末尾作为结果提交，由你判断是否合并更改。你负责制定一系列方案和最终执行/审查。你始终是主架构员。
给子代理的提示词里要求它把这次实际执行的操作、思路和结果写进最终返回。结果回来后，你对照这些记录核验有没有错、要不要再查。不要把原始长记录原样复述给用户，除非用户要求。
subagents 只在后台跑。这一轮会结束，跑完后结果自动送回。启动后你看不到实时过程。用户之后要求查看时，一次只用 subagents_control 的 read 读取一个子任务的末尾最多 8000 字符；read 要明确提供 run_id 和 label，先用 list/status 确认目标，不要假定空参数会自动选择最近任务。看完向用户汇报状态，不要一次假定能拿到所有子代理的全文。]`

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
