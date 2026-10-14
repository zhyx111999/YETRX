package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/workflow"
)

func (s *Server) initWorkflows() {
	if s.m.pg == nil {
		return
	}
	if err := s.upgradeWorkflowSchemas(); err != nil {
		log.Printf("[workflow] tool schema upgrade: %v", err)
	}
	if err := s.upgradeDelegationDescriptions(); err != nil {
		log.Printf("[delegation] tool description upgrade: %v", err)
	}
	manager, err := workflow.New(s.ctx, filepath.Join(s.m.dir, "workflows"), workflow.DefaultConcurrency, s.runWorkflowTask, s.workflowCompleted)
	if err != nil {
		log.Printf("[workflow] init: %v", err)
		return
	}
	s.workflows = manager
	s.seedWorkflowSkills()
}

// Like upstream's other skill seeds, this is a one-time default binding. The
// files live exclusively in the global skillDir and user binding edits survive.
func (s *Server) seedWorkflowSkills() {
	const flag = "native_workflow_skills_v1"
	if value, _, _ := s.m.pg.GetSetting(flag); value == "true" {
		return
	}
	for _, name := range []string{"workflow-authoring", "workflow-patterns", "subagents-setup"} {
		if _, err := os.Stat(filepath.Join(s.skillDir, name, "SKILL.md")); err != nil {
			return
		}
		if _, err := s.m.pg.Exec(`INSERT INTO agent_skill_visibility (agent_id,skill_name,enabled)
SELECT id,$1,true FROM agents WHERE key='agent' AND builtin ON CONFLICT DO NOTHING`, name); err != nil {
			log.Printf("[workflow] skill binding %s: %v", name, err)
			return
		}
	}
	if err := s.m.pg.SetSetting(flag, "true"); err != nil {
		log.Printf("[workflow] skill seed marker: %v", err)
	}
}

func (s *Server) runWorkflowTask(ctx context.Context, job workflow.Job) (string, error) {
	id, err := conversationSessionID(job.OwnerSession)
	if err != nil {
		return "", err
	}
	parent, err := s.m.pg.GetConversation(id)
	if err != nil {
		return "", err
	}
	if parent == nil {
		return "", workflow.Permanent(fmt.Errorf("parent conversation no longer exists"))
	}
	key := job.Task.AgentKey
	if key == "" {
		key = parent.AgentKey
	}
	mode, err := s.m.pg.GetAgentByKey(key)
	if err != nil {
		return "", err
	}
	if mode == nil || !mode.Enabled {
		return "", workflow.Permanent(fmt.Errorf("mode %q is unavailable", key))
	}
	// A workflow task inherits the exact ChatAgent selected by its parent
	// conversation. Its AgentKey still controls the child prompt/tools, but the
	// provider, profile binding, and global fallback stay those of the current
	// session. Subagents have no separate model override.
	ca := s.resolveChatAgent(parent)
	if ca == nil {
		return "", workflow.Permanent(fmt.Errorf("%s", s.chatUnavailableReason()))
	}
	session := job.SessionID
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "%s\n\n当前源码工作目录：%s\n最终说明实际操作、依据、文件和命令、测试结果、失败及未验证项，供主对话审查。", job.Task.Task, job.CWD)
	if job.Task.Isolation == "worktree" {
		prompt.WriteString("本任务已创建独立 Git worktree；不要自动合并，报告工作树路径、分支和提交。")
	}
	for _, label := range job.Task.DependsOn {
		fmt.Fprintf(&prompt, "\n\n<dependency label=%q>\n%s\n</dependency>", label, job.Results[label])
	}
	ctx = context.WithValue(intercept.WithConvID(ctx, id), workflowCallerKey{}, workflowCaller{Session: job.OwnerSession, Agent: job.OwnerAgent, Depth: job.Depth})
	emit := func(activity db.Activity) {
		activity.Worker = "subagent:" + job.Task.Label
		if _, err := s.m.pg.AppendConvActivity(id, activity); err != nil {
			log.Printf("[workflow %s] activity: %v", job.RunID, err)
		}
	}
	ctx = agent.WithChatControl(ctx, agent.ChatControl{WorkingDirectory: job.CWD, Child: true, DrainMessages: job.Messages.Drain, RollbackOnError: job.Task.Thread != ""})
	text, err := ca.Chat(ctx, key, session, prompt.String(), mode.MaxTurns, time.Duration(mode.RunSecs)*time.Second, mode.WebSearch, emit)
	if errors.Is(err, agent.ErrChatBudget) || errors.Is(err, agent.ErrChatRollback) {
		err = workflow.Permanent(err)
	} else if errors.Is(err, agent.ErrChatEmpty) {
		err = &workflow.Failure{Code: "AGENT_EMPTY_OUTPUT", Recoverable: true, Cause: err}
	}
	return text, err
}

// WaitBackground lets the native runner finish cancellation before its stores
// close. The caller's normal HTTP shutdown deadline bounds this wait.
func (s *Server) WaitBackground(ctx context.Context) {
	if s.workflows == nil {
		return
	}
	done := make(chan struct{})
	go func() { s.workflows.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
