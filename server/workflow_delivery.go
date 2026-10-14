package server

import (
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
)

func workflowReport(run workflow.Run) string {
	var body strings.Builder
	fmt.Fprintf(&body, "子代理运行 %s：%s\n", run.ID, run.Status)
	if run.Checkpoint != nil && run.Checkpoint.Status == "waiting" {
		fmt.Fprintf(&body, "等待检查点 %s（%s）：%s\n使用 subagents_control resume，提供 checkpoint_id 和 response 回答后继续。\n", run.Checkpoint.ID, run.Checkpoint.Kind, run.Checkpoint.Payload)
	}
	if run.Status == workflow.Paused && run.PauseReason == "usage_limit" {
		fmt.Fprintf(&body, "提供商额度或速率限制导致暂停；自动恢复次数 %d/5，计划时间 %s。也可手动 resume 或 stop。\n", min(run.AutoResumeAttempts, 5), run.ResumeAt.Format("2006-01-02 15:04:05Z07:00"))
	}
	quota := min(3000, 16000/max(1, len(run.Tasks)))
	for _, task := range run.Tasks {
		fmt.Fprintf(&body, "\n[%s] %s\n", task.Label, task.Status)
		if task.Workspace != nil && task.Workspace.Isolated {
			fmt.Fprintf(&body, "worktree: %s；分支：%s；已删除：%t\n", task.Workspace.CWD, task.Workspace.Branch, task.Workspace.Removed)
			if task.Workspace.CleanupError != "" {
				fmt.Fprintf(&body, "清理失败：%s\n", task.Workspace.CleanupError)
			}
		}
		if task.Draining {
			body.WriteString("旧执行器尚未退出；退出前禁止恢复和重试。\n")
		}
		text := task.Result
		if task.Error != "" {
			text += "\n" + task.Error
		}
		runes := []rune(text)
		if len(runes) > quota {
			text = "[末尾摘要；完整结果可用 subagents_control read 读取]\n" + string(runes[len(runes)-quota:])
		}
		body.WriteString(text)
		body.WriteByte('\n')
	}
	return body.String()
}

func (s *Server) workflowCompleted(run workflow.Run) {
	body := workflowReport(run)
	prompt := body
	if run.Status == workflow.Stopped || run.Status == workflow.Interrupted {
		prompt = ""
	}
	s.queueWorkflowDelivery(run.OwnerSession, workflowDelivery{
		Activity: db.Activity{Kind: "text", Tool: "subagents", Summary: "子代理状态更新", Detail: body},
		Prompt:   prompt,
	})
}
