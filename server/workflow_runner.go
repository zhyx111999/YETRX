package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/workflow"
)

func (s *Server) initWorkflows() {
	if s.m.pg == nil {
		return
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
		return "", fmt.Errorf("parent conversation no longer exists")
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
		return "", fmt.Errorf("mode %q is unavailable", key)
	}
	ca := s.resolveChatAgent(&db.Conversation{AgentKey: key, LLMProfileID: parent.LLMProfileID})
	if job.Task.LLMProfileID != nil {
		ca = s.chatAgentForProfile(*job.Task.LLMProfileID)
	}
	if ca == nil {
		return "", fmt.Errorf("%s", s.chatUnavailableReason())
	}
	digest := sha256.Sum256([]byte(job.Task.Label))
	session := fmt.Sprintf("workflow-%s-%x", job.RunID, digest[:8])
	workspace, err := filepath.Abs(filepath.Join(s.m.dir, "sessions", job.OwnerSession))
	if err != nil {
		return "", err
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "%s\n\n父对话工作目录：%s\n本任务有独立工作目录；修改共享项目时先创建独立副本，最终说明实际操作、依据和结果，供主对话审查。", job.Task.Task, workspace)
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
	return ca.Chat(ctx, key, session, prompt.String(), mode.MaxTurns, time.Duration(mode.RunSecs)*time.Second, mode.WebSearch, emit)
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
