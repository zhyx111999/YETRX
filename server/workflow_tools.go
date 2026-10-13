package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/workflow"
	actool "github.com/Autumn-27/norma/tool"
)

type workflowCallerKey struct{}
type workflowCaller struct {
	Session, Agent string
	Depth          int
}

func (s *Server) workflowOwner(ctx context.Context) (workflowCaller, error) {
	if owner, ok := ctx.Value(workflowCallerKey{}).(workflowCaller); ok {
		return owner, nil
	}
	session := agent.RunInfoFrom(ctx).SessionID
	id, err := conversationSessionID(session)
	if err != nil {
		return workflowCaller{}, err
	}
	c, err := s.m.pg.GetConversation(id)
	if err != nil {
		return workflowCaller{}, err
	}
	if c == nil {
		return workflowCaller{}, errors.New("conversation no longer exists")
	}
	return workflowCaller{Session: session, Agent: c.AgentKey}, nil
}

func conversationSessionID(session string) (int64, error) {
	if !strings.HasPrefix(session, "conv-") {
		return 0, errors.New("background tools require a conversation session")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(session, "conv-"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid conversation session")
	}
	return id, nil
}

// Host tools use the same global catalog, description overrides and mode
// bindings as the existing orchestration tools. No Agent-specific dispatch.
func (s *Server) workflowTools() []actool.CoreTool {
	return []actool.CoreTool{
		wrTool("subagents", "在后台启动独立子任务。立即返回 run_id，结束后结果自动回到当前对话。子任务继承当前会话实际使用的模型配置、全局资源和上下文约束；depends_on 可连接前后步骤。", objSchema(map[string]any{
			"tasks": map[string]any{"type": "array", "minItems": 1, "maxItems": workflow.MaxTasks, "items": objSchema(map[string]any{
				"label": strParam("本批次唯一名称"), "task": strParam("完整任务，说明输入、范围和预期结果"),
				"agent_key":  strParam("全局模式 key；省略则沿用当前模式"),
				"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "先完成的任务 label；成功结果会交给本任务"},
			}, "label", "task")},
		}, "tasks"), s.startWorkflow),
		wrTool("subagents_control", "管理当前对话的后台任务：list/status/read/stop/resume。read 一次读取一个子任务，最多返回末尾 8000 字符；resume 保留成功结果，仅重试未完成部分。", objSchema(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"list", "status", "read", "stop", "resume"}},
			"run_id": strParam("subagents 返回的 run_id"), "label": strParam("read 指定一个子任务"),
			"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": workflow.MaxReadChars, "default": workflow.MaxReadChars},
		}, "action"), s.controlWorkflow),
	}
}

func (s *Server) startWorkflow(ctx context.Context, input json.RawMessage) (actool.Result, error) {
	if s.workflows == nil {
		return actool.Errorf("后台任务管理器未就绪"), nil
	}
	owner, err := s.workflowOwner(ctx)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var request struct {
		Tasks []workflow.Task `json:"tasks"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	for i := range request.Tasks {
		task := &request.Tasks[i]
		if strings.TrimSpace(task.AgentKey) == "" {
			task.AgentKey = owner.Agent
		}
		a, err := s.m.pg.GetAgentByKey(task.AgentKey)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if a == nil || !a.Enabled {
			return actool.Errorf("模式不存在或未启用: " + task.AgentKey), nil
		}
	}
	s.chatMu.Lock()
	if err := ctx.Err(); err != nil {
		s.chatMu.Unlock()
		return actool.Errorf(err.Error()), nil
	}
	id, err := s.workflows.Start(owner.Session, owner.Agent, owner.Depth, request.Tasks)
	if err == nil {
		delete(s.workflowMuted, owner.Session)
	}
	s.chatMu.Unlock()
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return jsonResult(map[string]any{"run_id": id, "status": "running", "message": "任务已在后台启动，完成后会自动返回当前对话。"})
}

func (s *Server) controlWorkflow(ctx context.Context, input json.RawMessage) (actool.Result, error) {
	if s.workflows == nil {
		return actool.Errorf("后台任务管理器未就绪"), nil
	}
	owner, err := s.workflowOwner(ctx)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var request struct {
		Action   string `json:"action"`
		RunID    string `json:"run_id"`
		Label    string `json:"label"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if request.Action == "list" {
		items := []any{}
		for _, run := range s.workflows.List(owner.Session) {
			items = append(items, workflowSummary(run))
		}
		return jsonResult(items)
	}
	if request.Action == "read" {
		text, err := s.workflows.Read(owner.Session, request.RunID, request.Label, request.MaxChars)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return actool.Text(text), nil
	}
	var run workflow.Run
	switch request.Action {
	case "status":
		run, err = s.workflows.Status(owner.Session, request.RunID)
	case "stop":
		run, err = s.workflows.Stop(owner.Session, request.RunID)
	case "resume":
		s.chatMu.Lock()
		if err = ctx.Err(); err == nil {
			run, err = s.workflows.Resume(owner.Session, request.RunID)
			if err == nil {
				delete(s.workflowMuted, owner.Session)
			}
		}
		s.chatMu.Unlock()
	default:
		err = fmt.Errorf("unsupported action %q", request.Action)
	}
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return jsonResult(workflowSummary(run))
}

func workflowSummary(run workflow.Run) map[string]any {
	tasks := []any{}
	for _, task := range run.Tasks {
		tasks = append(tasks, map[string]any{"label": task.Label, "agent_key": task.AgentKey, "status": task.Status, "error": task.Error})
	}
	return map[string]any{"run_id": run.ID, "status": run.Status, "tasks": tasks, "updated_at": run.UpdatedAt}
}
