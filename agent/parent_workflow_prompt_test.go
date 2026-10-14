package agent

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type parentPromptProvider struct {
	request llm.CompletionRequest
}

func (p *parentPromptProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.request = req
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}}, "end_turn", llm.Usage{}, nil
}

func (p *parentPromptProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		message, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for _, block := range message.Content {
			if block.Type == llm.BlockText && !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: block.Text}, nil) {
				return
			}
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestChatParentWorkflowPromptStaysAtRequestBoundary(t *testing.T) {
	cases := []struct {
		name         string
		marked       bool
		workflowTool bool
		child        bool
	}{
		{name: "ordinary", marked: false},
		{name: "human-turn-with-subagents", marked: true, workflowTool: true},
		{name: "human-turn-without-subagents", marked: true},
		{name: "child-turn", marked: true, workflowTool: true, child: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &parentPromptProvider{}
			chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 100000)
			chat.SetNonStreaming(func() bool { return true })
			oldAugment, oldResolve := ToolAugment, ToolResolve
			t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
			if tc.workflowTool {
				ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
					return []actool.CoreTool{actool.Build(actool.Spec{
						Name:   "subagents",
						Schema: map[string]any{"type": "object"},
					})}, DeferredInfo{}, func() {}
				}
				ToolResolve = nil
			}
			ctx := context.Background()
			if tc.marked {
				ctx = WithParentWorkflowPrompt(ctx)
			}
			if tc.child {
				ctx = WithChatControl(ctx, ChatControl{Child: true})
			}
			if _, err := chat.Chat(ctx, "agent", "conv-parent-prompt", "ORIGINAL_USER_TEXT", 2, time.Minute, false, func(db.Activity) {}); err != nil {
				t.Fatal(err)
			}
			foundUser := false
			for _, message := range provider.request.Messages {
				for _, block := range message.Content {
					if block.Type == llm.BlockText && block.Text == "ORIGINAL_USER_TEXT" {
						foundUser = true
					}
				}
			}
			if !foundUser {
				t.Fatalf("original user message missing or changed: %+v", provider.request.Messages)
			}
			system := strings.Join(provider.request.System, "\n")
			got := strings.Contains(system, ParentWorkflowPrompt)
			want := tc.marked && tc.workflowTool && !tc.child
			if got != want {
				t.Fatalf("parent prompt present=%v, want %v; system=%q", got, want, system)
			}
			if strings.Contains(system, SubagentContext) != tc.child || strings.Contains(system, ExplorationSubtaskContext) {
				t.Fatalf("delegation context mixed up: %q", system)
			}
			for _, message := range provider.request.Messages {
				if strings.Contains(message.Text(), SubagentContext) || strings.Contains(message.Text(), ParentWorkflowPrompt) {
					t.Fatal("runtime delegation context leaked into message history")
				}
			}
		})
	}
}
