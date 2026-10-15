package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

// These tests need no database: the Manager has no PG handle, so a request that
// slipped past the guard would panic inside task creation instead of returning.

func callSpawnTask(t *testing.T, s *Server, ctx context.Context, input string) actool.Result {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("spawn_task passed the guard and reached task creation: %v", r)
		}
	}()
	res, err := s.toolSpawnTask().Call(ctx, json.RawMessage(input), &actool.ToolContext{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSpawnTaskRejectsChildContextsWithoutCreatingTasks(t *testing.T) {
	child := agent.WithChatControl(context.Background(), agent.ChatControl{Child: true})
	depth := context.WithValue(context.Background(), workflowCallerKey{}, workflowCaller{Session: "conv-1", Agent: "agent", Depth: 1})
	for name, ctx := range map[string]context.Context{
		"chat-control-only":    child,
		"workflow-depth-only":  depth,
		"both-markers":         context.WithValue(child, workflowCallerKey{}, workflowCaller{Session: "conv-1", Agent: "agent", Depth: 1}),
		"child-nested-in-main": agent.WithChatControl(context.WithValue(context.Background(), workflowCallerKey{}, workflowCaller{Session: "conv-1", Agent: "agent"}), agent.ChatControl{Child: true}),
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{m: &Manager{tasks: map[string]*Task{}}}
			// Valid input proves the guard stops creation; missing goal proves it runs before validation.
			for _, input := range []string{`{"description":"d","goal":"g"}`, `{"description":"d"}`} {
				res := callSpawnTask(t, s, ctx, input)
				if !res.IsError || !strings.Contains(res.Flatten(), "子代理不能创建探索子任务") {
					t.Fatalf("input %s not rejected by guard: %+v", input, res)
				}
			}
			if len(s.m.tasks) != 0 || s.m.active != "" {
				t.Fatalf("task created: tasks=%d active=%q", len(s.m.tasks), s.m.active)
			}
		})
	}
}

// The main conversation (including the owner context of a subagent run, Depth 0)
// passes the guard and reaches the handler's own validation.
func TestSpawnTaskGuardLeavesMainConversationAlone(t *testing.T) {
	owner := context.WithValue(context.Background(), workflowCallerKey{}, workflowCaller{Session: "conv-1", Agent: "agent"})
	for name, ctx := range map[string]context.Context{
		"plain":                 context.Background(),
		"non-child-chat":        agent.WithChatControl(context.Background(), agent.ChatControl{WorkingDirectory: t.TempDir()}),
		"workflow-owner-depth0": owner,
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{m: &Manager{tasks: map[string]*Task{}}}
			res := callSpawnTask(t, s, ctx, `{"description":"d"}`)
			if !res.IsError || res.Flatten() != "goal 为必填" {
				t.Fatalf("main conversation did not reach handler validation: %+v", res)
			}
			if len(s.m.tasks) != 0 {
				t.Fatalf("unexpected task: %d", len(s.m.tasks))
			}
		})
	}
}
