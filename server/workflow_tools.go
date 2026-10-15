package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		if owner.Depth > 0 {
			return workflowCaller{}, errors.New("subagents cannot dispatch or control workflows")
		}
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

func workflowTaskSchema() map[string]any {
	return map[string]any{"type": "array", "minItems": 1, "maxItems": workflow.MaxTasks, "items": objSchema(map[string]any{
		"label": strParam("本批次唯一名称"), "task": strParam("完整任务：输入、范围、预算和预期结果；checkpoint 步骤省略"),
		"agent_key":     strParam("全局模式 key；只决定提示词和资源，模型继承当前会话"),
		"depends_on":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "前置步骤 label；成功结果传入本任务"},
		"cwd":           strParam("共享源码目录；相对运行 cwd 解析；不能与 isolation 同时使用"),
		"isolation":     map[string]any{"type": "string", "enum": []string{"worktree"}, "description": "在运行 cwd 的 Git HEAD 创建独立分支/worktree；失败则终止任务"},
		"keep_worktree": map[string]any{"type": "boolean", "default": true, "description": "默认保留供审查；false 在执行器退出后删除工作树和分支，不自动合并"},
		"thread":        strParam("本次运行内的命名子会话；同名任务用 depends_on 串行，cwd 必须一致；恢复时重新执行这些回合"),
		"checkpoint":    objSchema(map[string]any{"checkpoint_id": strParam("运行内唯一 ID"), "kind": strParam("检查点种类"), "payload": map[string]any{"description": "供主代理处理的 JSON 数据"}}, "checkpoint_id", "kind", "payload"),
	}, "label")}
}

// Ordinary globally registered host tools; child execution strips both names
// and workflowOwner also denies their handlers, including indirect calls.
func (s *Server) workflowTools() []actool.CoreTool {
	return []actool.CoreTool{
		wrTool("subagents", "按需启动子代理：用独立会话完成一次明确的求证、审查或小范围修改，返回结果供主对话核验，不负责围绕探索目标持续规划。需要持续探索时使用 spawn_task 子任务。立即返回 run_id；结束或暂停自动回传。模型继承当前会话；默认共享 cwd，改源码时用 isolation=worktree 创建独立副本，不自动合并。", objSchema(map[string]any{
			"tasks": workflowTaskSchema(), "cwd": strParam("运行源码根目录；省略使用父对话工作目录"),
			"agent_retries":    map[string]any{"type": "integer", "minimum": 0, "maximum": workflow.MaxAgentRetries, "default": 0, "description": "可恢复失败后的额外尝试次数；等待旧执行器停止后重试"},
			"agent_timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483647, "default": 0, "description": "每次尝试的硬超时；0 不另加超时，模式预算仍有效"},
		}, "tasks"), s.startWorkflow),
		wrTool("subagents_control", "管理当前对话的后台子代理（run_id/label），不控制探索子任务（task_id/intent_id）。message 向正在执行的子代理排队指令，在下次模型调用前送达，提前结束则丢弃。resume 可提交编辑后的 tasks，复用未变成功步骤；变更及其下游重跑。检查点必须给 checkpoint_id 和 response。read 明确提供 run_id/label，一次读取一个子代理，最多末尾 8000 字符。", objSchema(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"list", "status", "read", "stop", "resume", "message"}},
			"run_id": strParam("subagents 返回的 run_id"), "label": strParam("read/message 的单个子代理 label"),
			"text": strParam("追加指令原文"), "tasks": workflowTaskSchema(), "checkpoint_id": strParam("当前等待响应的检查点 ID"),
			"response":  map[string]any{"description": "检查点响应，任意 JSON，包括 null"},
			"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": workflow.MaxReadChars, "default": workflow.MaxReadChars},
		}, "action"), s.controlWorkflow),
	}
}

func (s *Server) validateWorkflowModes(tasks []workflow.Task, owner workflowCaller) error {
	for i := range tasks {
		task := &tasks[i]
		if task.Checkpoint != nil {
			continue
		}
		if strings.TrimSpace(task.AgentKey) == "" {
			task.AgentKey = owner.Agent
		}
		a, err := s.m.pg.GetAgentByKey(task.AgentKey)
		if err != nil {
			return err
		}
		if a == nil || !a.Enabled {
			return fmt.Errorf("模式不存在或未启用: %s", task.AgentKey)
		}
	}
	return nil
}

func (s *Server) startWorkflow(ctx context.Context, input json.RawMessage) (actool.Result, error) {
	if s.workflows == nil {
		return actool.Errorf("子代理管理器未就绪"), nil
	}
	owner, err := s.workflowOwner(ctx)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var request struct {
		Tasks []workflow.Task `json:"tasks"`
		workflow.Options
	}
	conversationID, _ := conversationSessionID(owner.Session)
	if err := s.checkRetestDelegation(ctx, conversationID); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if err = json.Unmarshal(input, &request); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if err = s.validateWorkflowModes(request.Tasks, owner); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	parentCWD := filepath.Join(s.m.dir, "sessions", owner.Session)
	if err = os.MkdirAll(parentCWD, 0755); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if request.CWD == "" {
		request.CWD = parentCWD
	} else if !filepath.IsAbs(request.CWD) {
		request.CWD = filepath.Join(parentCWD, request.CWD)
	}
	s.chatMu.Lock()
	if err = ctx.Err(); err != nil {
		s.chatMu.Unlock()
		return actool.Errorf(err.Error()), nil
	}
	id, err := s.workflows.StartWithOptions(owner.Session, owner.Agent, owner.Depth, request.Tasks, request.Options)
	if err == nil {
		delete(s.workflowMuted, owner.Session)
	}
	s.chatMu.Unlock()
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return jsonResult(map[string]any{"run_id": id, "status": "running", "message": "子代理已在后台启动；结果在主对话忙碌时排队，本轮输出结束后立即送入供主代理审查。"})
}

func (s *Server) controlWorkflow(ctx context.Context, input json.RawMessage) (actool.Result, error) {
	if s.workflows == nil {
		return actool.Errorf("子代理管理器未就绪"), nil
	}
	owner, err := s.workflowOwner(ctx)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var request struct {
		Action   string `json:"action"`
		RunID    string `json:"run_id"`
		Label    string `json:"label"`
		Text     string `json:"text"`
		MaxChars int    `json:"max_chars"`
		workflow.ResumeOptions
	}
	if err = json.Unmarshal(input, &request); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	switch request.Action {
	case "list":
		items := []any{}
		for _, run := range s.workflows.List(owner.Session) {
			items = append(items, workflowSummary(run))
		}
		return jsonResult(items)
	case "read":
		text, err := s.workflows.Read(owner.Session, request.RunID, request.Label, request.MaxChars)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return actool.Text(text), nil
	case "message":
		if err = s.workflows.Message(owner.Session, request.RunID, request.Label, request.Text); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"run_id": request.RunID, "label": request.Label, "result": "queued", "message": "指令将在下次模型调用前送达；若子代理先结束，则丢弃。"})
	}
	var run workflow.Run
	switch request.Action {
	case "status":
		run, err = s.workflows.Status(owner.Session, request.RunID)
	case "stop":
		run, err = s.workflows.Stop(owner.Session, request.RunID)
	case "resume":
		if request.Tasks != nil {
			if err = s.validateWorkflowModes(request.Tasks, owner); err != nil {
				return actool.Errorf(err.Error()), nil
			}
		}
		s.chatMu.Lock()
		if err = ctx.Err(); err == nil {
			run, err = s.workflows.ResumeWithOptions(owner.Session, request.RunID, request.ResumeOptions)
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
		tasks = append(tasks, map[string]any{"label": task.Label, "agent_key": task.AgentKey, "status": task.Status, "error": task.Error,
			"attempts": task.Attempts, "attempt_history": task.AttemptHistory, "session_id": task.SessionID, "workspace": task.Workspace, "draining": task.Draining})
	}
	return map[string]any{"run_id": run.ID, "status": run.Status, "tasks": tasks, "updated_at": run.UpdatedAt, "revision": run.Revision, "checkpoint": run.Checkpoint, "pause_reason": run.PauseReason, "auto_resume_attempts": run.AutoResumeAttempts, "resume_at": run.ResumeAt, "reset_hint": run.ResetHint}
}
