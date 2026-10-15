package agentexec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// Exercise the production ChatControl wrapper with the real child process,
// including response flush, same-session steering and accepted-message ACKs.
func TestChatResponseBoundaryProcess(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		t.Run(fmt.Sprint(nonstream), func(t *testing.T) {
			factory := NewFactory(testConfig())
			var active agent.RuntimeSession
			agent.SetSessionFactory(func(ctx context.Context, opts agentcore.Options) (agent.RuntimeSession, error) {
				s, err := factory(ctx, opts)
				active = s
				return s, err
			})
			defer agent.SetSessionFactory(nil)
			root := t.TempDir()
			store := transcript.NewStore(root)
			var queue []string
			requests, prompts, boundaries := 0, 0, 0
			var activities []db.Activity
			p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				requests++
				if requests == 1 {
					return toolsResponse(llm.TextBlock("response text before tool"), use("write", "Write", `{"file_path":"boundary.txt","content":"child"}`), llm.TextBlock("response tail"))
				}
				user, result := -1, -1
				for i, m := range r.Messages {
					if m.Text() == "delivered result" {
						user = i
					}
					for _, b := range m.Content {
						if b.Type == llm.BlockToolResult && b.ToolUseID == "write" {
							result = i
						}
					}
				}
				if user <= result || result < 0 {
					return llm.Message{}, "", llm.Usage{}, fmt.Errorf("steering split tool protocol: %+v", r.Messages)
				}
				snapshot := active.Messages()
				if snapshot[len(snapshot)-1].Text() != "delivered result" {
					return llm.Message{}, "", llm.Usage{}, fmt.Errorf("next request saw stale host snapshot: %+v", snapshot)
				}
				return textResponse("finished")
			}}
			chat := agent.NewChatAgent(p, "fixture", root, store, 100000)
			chat.SetNonStreaming(func() bool { return nonstream })
			ctx := agent.WithChatControl(context.Background(), agent.ChatControl{WorkingDirectory: root, Child: true,
				DrainMessages: func() []string { out := queue; queue = nil; return out },
				OnPrompt: func() {
					prompts++
					messages := active.Messages()
					if len(messages) == 0 {
						t.Error("ACK before host snapshot")
					}
					records, err := store.Load(store.MainPath("boundary"))
					if err != nil {
						t.Error(err)
						return
					}
					persisted, _ := transcript.Messages(records)
					if len(persisted) == 0 || persisted[len(persisted)-1].Text() != messages[len(messages)-1].Text() {
						t.Error("ACK before child transcript append")
					}
				},
				OnResponseEnd: func() {
					boundaries++
					if requests != 1 {
						t.Error("response callback at wrong request")
					}
					tail := false
					for _, a := range activities {
						if strings.Contains(a.Detail, "response tail") {
							tail = true
						}
					}
					if !tail {
						t.Error("response callback before capture flush")
					}
					queue = []string{"delivered result"}
				},
			})
			answer, err := chat.Chat(ctx, "agent", "boundary", "start", 8, time.Minute, false, func(a db.Activity) { activities = append(activities, a) })
			if err != nil || answer != "finished" || requests != 2 || prompts != 2 || boundaries != 1 {
				t.Fatalf("answer=%q err=%v req=%d prompt=%d boundaries=%d", answer, err, requests, prompts, boundaries)
			}
			for _, a := range activities {
				if a.IsError {
					t.Fatalf("internal steering leaked: %+v", a)
				}
			}
		})
	}
}
func TestChatCanceledDrainIsNotAcknowledged(t *testing.T) {
	agent.SetSessionFactory(NewFactory(testConfig()))
	defer agent.SetSessionFactory(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drains, prompts, calls := 0, 0, 0
	p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return textResponse("unexpected")
	}}
	root := t.TempDir()
	chat := agent.NewChatAgent(p, "fixture", root, transcript.NewStore(root), 100000)
	ctx = agent.WithChatControl(ctx, agent.ChatControl{WorkingDirectory: root, Child: true, DrainMessages: func() []string { drains++; cancel(); return []string{"never accepted"} }, OnPrompt: func() { prompts++ }})
	_, err := chat.Chat(ctx, "agent", "canceled", "first", 8, time.Minute, false, nil)
	if err == nil || drains != 1 || prompts != 1 || calls != 0 {
		t.Fatalf("canceled batch ACK: err=%v drains=%d prompts=%d calls=%d", err, drains, prompts, calls)
	}
}
