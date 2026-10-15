package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// The factory and Prompt deliberately share a context, as in production Chat.
// No host history is supplied on Resume: only the real child-written transcript.
func TestCanceledResponseTranscriptResume(t *testing.T) {
	for _, mode := range []string{"cancel", "break", "close", "host-ignores-cancel", "child-ignores-cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), testKey{}, "owner"))
			defer cancel()
			store := transcript.NewStore(t.TempDir())
			completed, toolEntered, toolCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			var calls atomic.Int32
			name := "Hold"
			if mode == "child-ignores-cancel" {
				name = "Hang"
			}
			host := tool.Build(tool.Spec{Name: name, Schema: objectSchema(), Run: func(ctx context.Context, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
				if ctx.Value(testKey{}) != "owner" || transcript.SessionIDFrom(ctx) != "stopped" {
					t.Error("lost host identity")
				}
				close(toolEntered)
				<-completed
				tc.Emit(tool.ProgressInfo{Message: "response completed"})
				if mode == "host-ignores-cancel" {
					<-release
				}
				<-ctx.Done()
				close(toolCanceled)
				return tool.Result{}, ctx.Err()
			}})
			p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls.Add(1)
				return toolsResponse(llm.TextBlock("Starting"), use("hold", name, `{}`))
			}}
			opts := agentcore.Options{Provider: p, Tools: []tool.CoreTool{host}, PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true, Transcript: store, SessionID: "stopped"}
			opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					p.Stream(ctx, req)(yield)
					close(completed)
				}
			}
			config := testConfig()
			if mode == "child-ignores-cancel" {
				config.LocalTool = func(tool.CoreTool) (*LocalToolSpec, error) {
					return &LocalToolSpec{Kind: "test", Data: json.RawMessage(`"Hang"`)}, nil
				}
				config.OnLocalToolStart = func(context.Context, tool.CoreTool) { close(toolEntered) }
			}
			rt, err := NewFactory(config)(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			s := rt.(*Session)
			defer s.Close()
			var terminal *harness.Terminal
			stopped := time.Time{}
			closed := make(chan error, 1)
			// For a child-local hanging tool there is no host progress RPC. Wait for
			// both model EOF and actual tool entry, then cancel an outstanding next.
			if mode == "child-ignores-cancel" {
				go func() { <-completed; <-toolEntered; cancel() }()
			}
			for ev, err := range s.Prompt(ctx, "start") {
				if err != nil {
					t.Fatalf("iterator: %v", err)
				}
				if ev.Kind == harness.KindProgress {
					stopped = time.Now()
					switch mode {
					case "break":
					case "close":
						go func() { closed <- s.Close() }()
					default:
						cancel()
					}
					if mode == "break" {
						break
					}
				}
				if ev.Terminal != nil {
					terminal = ev.Terminal
				}
			}
			if mode == "close" {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			if mode != "break" && (terminal == nil || terminal.Reason != harness.ReasonAbortedTools || !errors.Is(terminal.Err, context.Canceled)) {
				t.Fatalf("terminal: %+v", terminal)
			}
			if calls.Load() != 1 {
				t.Fatalf("model called after cancellation: %d", calls.Load())
			}
			if !stopped.IsZero() && time.Since(stopped) > 3*time.Second {
				t.Fatal("unbounded cancellation/drain")
			}
			if mode != "host-ignores-cancel" && mode != "child-ignores-cancel" {
				select {
				case <-toolCanceled:
				case <-time.After(time.Second):
					t.Fatal("host tool lost business cancellation")
				}
			}
			closeStarted := time.Now()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if time.Since(closeStarted) > 3*time.Second {
				t.Fatal("Close exceeded tool cleanup bound")
			}
			records, err := store.Load(store.MainPath("stopped"))
			if err != nil {
				t.Fatal(err)
			}
			history, _ := transcript.Messages(records)
			assertStoppedHistory(t, history)
			opts.Deps.CallModel = nil
			opts.Provider = fakeProvider{func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				history := req.Messages[:len(req.Messages)-1]
				for len(history) > 0 && history[0].Text() != "start" {
					history = history[1:]
				}
				assertStoppedHistory(t, history)
				if req.Messages[len(req.Messages)-1].Text() != "follow up" {
					t.Error("missing new input")
				}
				return textResponse("resumed")
			}}
			resumed := startTest(t, opts)
			if err := resumed.Resume("stopped"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(history, resumed.Messages()) {
				t.Fatal("resume changed official transcript")
			}
			if _, err := runTest(t, resumed, context.Background(), "follow up"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertStoppedHistory(t *testing.T, messages []llm.Message) {
	t.Helper()
	if len(messages) != 3 || messages[0].Text() != "start" || messages[1].Role != llm.RoleAssistant || messages[1].Text() != "Starting" {
		t.Errorf("completed response lost: %+v", messages)
		return
	}
	uses := messages[1].ToolUses()
	if len(uses) != 1 || uses[0].ID != "hold" || len(messages[2].Content) != 1 {
		t.Errorf("tool ordering: %+v", messages)
		return
	}
	result := messages[2].Content[0]
	text := fmt.Sprint(result.Content)
	if result.Type != llm.BlockToolResult || result.ToolUseID != "hold" || !result.IsError || (!strings.Contains(text, "cancel") && !strings.Contains(text, "interrupted")) {
		t.Errorf("missing cancellation result: %+v", result)
	}
}

func TestCancellationDuringStreamKeepsOfficialSemantics(t *testing.T) {
	for _, process := range []bool{false, true} {
		t.Run(fmt.Sprint(process), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := transcript.NewStore(t.TempDir())
			opts := agentcore.Options{Provider: fakeProvider{}, Tools: []tool.CoreTool{}, DisableBackgroundTasks: true, Transcript: store, SessionID: "partial"}
			opts.Deps.CallModel = func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "unfinished"}, nil) {
						return
					}
					<-ctx.Done()
					yield(llm.StreamEvent{}, ctx.Err())
				}
			}
			var prompt func(context.Context, string) iter.Seq2[harness.Event, error]
			if process {
				s := startTest(t, opts)
				prompt = s.Prompt
			} else {
				s := agentcore.NewSession(opts)
				defer s.Close()
				prompt = s.Prompt
			}
			var terminal *harness.Terminal
			for ev, err := range prompt(ctx, "start") {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Kind == harness.KindText {
					cancel()
				}
				if ev.Terminal != nil {
					terminal = ev.Terminal
				}
			}
			if terminal == nil || terminal.Reason != harness.ReasonAbortedStreaming || !errors.Is(terminal.Err, context.Canceled) {
				t.Fatalf("terminal: %+v", terminal)
			}
			records, err := store.Load(store.MainPath("partial"))
			if err != nil {
				t.Fatal(err)
			}
			messages, _ := transcript.Messages(records)
			if len(messages) != 1 || messages[0].Text() != "start" {
				t.Fatalf("invented partial transcript: %+v", messages)
			}
		})
	}
}

func TestFactoryCancellationBeforeAndDuringPreparation(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(fmt.Sprint(during), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !during {
				cancel()
			}
			c := testConfig()
			c.Executable = "must-not-launch-agentexec"
			c.LocalTool = func(tool.CoreTool) (*LocalToolSpec, error) { cancel(); return nil, nil }
			rt, err := NewFactory(c)(ctx, agentcore.Options{Provider: fakeProvider{}, Tools: []tool.CoreTool{tool.Build(tool.Spec{Name: "Hold", Schema: objectSchema()})}})
			if rt != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled factory created session: %v %v", rt, err)
			}
		})
	}
}

func TestFactoryCancellationDuringInitIsNotAccepted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warned := false
	opts := agentcore.Options{Provider: fakeProvider{}, Tools: []tool.CoreTool{}, DisableBackgroundTasks: true,
		Compactor: &testCompactor{}, Compaction: &compaction.Config{}, OnWarn: func(string) { warned = true; cancel() }}
	rt, err := NewFactory(testConfig())(ctx, opts)
	if rt != nil {
		rt.Close()
		t.Fatal("acknowledged a canceled init")
	}
	if !warned || !errors.Is(err, context.Canceled) {
		t.Fatalf("init cancellation: warned=%v err=%v", warned, err)
	}
}
