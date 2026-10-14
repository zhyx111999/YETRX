package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

type controlledProvider struct {
	run func(llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p controlledProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.run(req)
}
func (p controlledProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for _, b := range msg.Content {
			switch b.Type {
			case llm.BlockText:
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
					return
				}
			case llm.BlockToolUse:
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			}
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}
func assistant(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}
}

func TestControlledChatSteeringPersistsAtNextBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "complete"}[stream], func(t *testing.T) {
			cwd, root := t.TempDir(), t.TempDir()
			tx := transcript.NewStore(filepath.Join(root, "transcripts"))
			var queue []string
			calls := 0
			provider := controlledProvider{run: func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls == 1 {
					queue = []string{"STEER: preserve this line\n  exact spacing."}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "write-once", Name: "Write", Input: json.RawMessage(`{"file_path":"changed.txt","content":"from child"}`)}}}, "tool_use", llm.Usage{OutputTokens: 3}, nil
				}
				if calls > 2 {
					t.Fatal("unexpected extra HTTP call")
				}
				directive, toolIndex := -1, -1
				for i, m := range req.Messages {
					if m.Text() == "STEER: preserve this line\n  exact spacing." {
						directive = i
					}
					for _, b := range m.Content {
						if b.Type == llm.BlockToolResult && b.ToolUseID == "write-once" {
							toolIndex = i
							if b.IsError {
								t.Fatalf("tool failed: %+v", b)
							}
						}
					}
				}
				if directive <= toolIndex || toolIndex < 0 {
					t.Fatalf("instruction not after completed tool: %+v", req.Messages)
				}
				return assistant("steered answer"), "end_turn", llm.Usage{OutputTokens: 4}, nil
			}}
			ca := NewChatAgent(provider, "fixture", root, tx, 100000)
			ca.SetNonStreaming(func() bool { return !stream })
			ctx := WithChatControl(context.Background(), ChatControl{WorkingDirectory: cwd, Child: true, DrainMessages: func() []string { out := queue; queue = nil; return out }})
			var results []db.Activity
			answer, err := ca.Chat(ctx, "agent", "child", "original task", 8, time.Minute, false, func(a db.Activity) {
				if a.Kind == "result" {
					results = append(results, a)
				}
			})
			if err != nil || answer != "steered answer" || calls != 2 {
				t.Fatalf("answer=%q calls=%d err=%v", answer, calls, err)
			}
			if len(results) != 1 || results[0].IsError || results[0].OutputTokens == nil || *results[0].OutputTokens != 7 {
				t.Fatalf("control boundary leaked or usage lost: %+v", results)
			}
			data, err := os.ReadFile(filepath.Join(cwd, "changed.txt"))
			if err != nil || string(data) != "from child" {
				t.Fatal("wrong execution cwd", err, string(data))
			}
			records, err := tx.Load(tx.MainPath("child"))
			if err != nil {
				t.Fatal(err)
			}
			messages, _ := transcript.Messages(records)
			count := 0
			for _, m := range messages {
				if m.Text() == "STEER: preserve this line\n  exact spacing." {
					count++
				}
				if strings.Contains(m.Text(), errSteeringBoundary.Error()) {
					t.Fatal("control signal entered transcript")
				}
			}
			if count != 1 {
				t.Fatalf("instruction persisted %d times", count)
			}
		})
	}
}

func TestChildCannotReachWorkflowToolsDirectlyOrIndirectly(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	defer func() { ToolAugment, ToolResolve = oldAugment, oldResolve }()
	calls := 0
	ToolResolve = nil
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		var tools []actool.CoreTool
		for _, name := range []string{"subagents", "subagents_control", "allowed_deferred"} {
			name := name
			tools = append(tools, actool.Build(actool.Spec{Name: name, Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				if name != "allowed_deferred" {
					calls++
				}
				return actool.Text("ok"), nil
			}}))
		}
		return tools, DeferredInfo{Deferred: []string{"subagents", "subagents_control", "allowed_deferred"}, GlobalNames: []string{"subagents", "allowed_deferred"}, Unlock: actool.NewUnlockSet("subagents", "subagents_control", "allowed_deferred")}, func() {}
	}
	turn := 0
	p := controlledProvider{run: func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		turn++
		for _, tool := range req.Tools {
			if tool.Name == "subagents" || tool.Name == "subagents_control" {
				t.Fatal("child schema exposes workflow tool")
			}
		}
		if turn == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockToolUse, ID: "direct", Name: "subagents", Input: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ID: "indirect", Name: "ExecuteExtraTool", Input: json.RawMessage(`{"tool_name":"subagents_control","arguments":{}}`)},
			}}, "tool_use", llm.Usage{}, nil
		}
		denied := 0
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolResult && b.IsError {
					denied++
				}
			}
		}
		if denied != 2 {
			t.Fatalf("blocked calls did not both fail: %+v", req.Messages)
		}
		return assistant("blocked"), "end_turn", llm.Usage{}, nil
	}}
	ca := NewChatAgent(p, "fixture", t.TempDir(), nil, 100000)
	ca.SetNonStreaming(func() bool { return true })
	_, err := ca.Chat(WithChatControl(context.Background(), ChatControl{Child: true}), "agent", "child", "try tools", 4, time.Minute, false, nil)
	if err != nil || calls != 0 {
		t.Fatal("child executed a forbidden handler", calls, err)
	}
}

func TestThreadFailureRollsBackHistoryButRetainsAudit(t *testing.T) {
	root := t.TempDir()
	tx := transcript.NewStore(filepath.Join(root, "transcripts"))
	turn := 0
	p := controlledProvider{run: func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		turn++
		if turn == 2 {
			return llm.Message{}, "", llm.Usage{}, errors.New("fixture failure")
		}
		if turn == 3 {
			for _, m := range req.Messages {
				if strings.Contains(m.Text(), "FAILED_TURN") {
					t.Fatal("failed turn leaked into thread")
				}
			}
		}
		return assistant("good"), "end_turn", llm.Usage{}, nil
	}}
	ca := NewChatAgent(p, "fixture", root, tx, 100000)
	ca.SetNonStreaming(func() bool { return true })
	ctx := WithChatControl(context.Background(), ChatControl{Child: true, RollbackOnError: true})
	if _, err := ca.Chat(ctx, "agent", "thread", "first", 2, time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(tx.MainPath("thread"))
	if _, err := ca.Chat(ctx, "agent", "thread", "FAILED_TURN", 2, time.Minute, false, nil); err == nil {
		t.Fatal("failure not returned")
	}
	after, _ := os.ReadFile(tx.MainPath("thread"))
	if string(before) != string(after) {
		t.Fatal("previous leaf was not restored")
	}
	archives, _ := filepath.Glob(tx.MainPath("thread") + ".failed-*")
	if len(archives) != 1 {
		t.Fatal("failed turn audit not retained")
	}
	if _, err := ca.Chat(ctx, "agent", "thread", "next", 2, time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
}
