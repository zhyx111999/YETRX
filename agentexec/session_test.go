package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENTEXEC_TEST_CHILD") == "1" {
		err := ServeWorker(context.Background(), WorkerConfig{LocalTools: map[string]LocalToolConstructor{"test": func(_ context.Context, raw json.RawMessage) (tool.CoreTool, error) {
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				return nil, err
			}
			return tool.Build(tool.Spec{Name: name, Schema: objectSchema(), Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
				if name == "Crash" {
					os.Exit(73)
				}
				if name == "Hang" {
					select {}
				}
				if name == "Wait" {
					<-ctx.Done()
					return tool.Result{}, ctx.Err()
				}
				return tool.Text(strconv.Itoa(os.Getpid())), nil
			}}), nil
		}}})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func objectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

type fakeProvider struct {
	run func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p fakeProvider) Complete(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.run(ctx, r)
}
func (p fakeProvider) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		m, stop, u, err := p.run(ctx, r)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for _, b := range m.Content {
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
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: u}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}
func textResponse(text string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{InputTokens: 4, OutputTokens: 2}, nil
}
func toolsResponse(blocks ...llm.ContentBlock) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: blocks}, "tool_use", llm.Usage{InputTokens: 4, OutputTokens: 2}, nil
}
func use(id, name, input string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}
func testConfig() Config {
	return Config{Args: []string{"-test.run=^$"}, Env: append(os.Environ(), "AGENTEXEC_TEST_CHILD=1", "ARTEX_CONFIG=C:/nonexistent-agentexec-child-config", "ARTEX_PG_DSN="), Limits: execution.Limits{MemoryBytes: 768 << 20, CPUs: 1, Processes: 32}}
}
func startTest(t *testing.T, opts agentcore.Options, config ...Config) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	c := testConfig()
	if len(config) > 0 {
		c = config[0]
	}
	rt, err := NewFactory(c)(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := rt.(*Session)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func runTest(t *testing.T, s *Session, ctx context.Context, input string) ([]harness.Event, error) {
	t.Helper()
	var events []harness.Event
	var failure error
	for ev, err := range s.Prompt(ctx, input) {
		events = append(events, ev)
		if err != nil {
			failure = err
			break
		}
		if ev.Terminal != nil && ev.Terminal.Err != nil {
			failure = ev.Terminal.Err
		}
	}
	return events, failure
}
func TestOptionsExplicitCoverage(t *testing.T) {
	classes := optionClasses()
	typ := reflect.TypeOf(agentcore.Options{})
	for i := 0; i < typ.NumField(); i++ {
		if classes[typ.Field(i).Name] == "" {
			t.Errorf("unclassified: %s", typ.Field(i).Name)
		}
	}
	_, _, err := prepare(agentcore.Options{Plan: &agentcore.PlanOptions{}, Memory: &agentcore.MemoryOptions{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "Plan, Memory") {
		t.Fatalf("unsupported fields: %v", err)
	}
}
func TestProcessStreamingAndComplete(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			var calls atomic.Int32
			p := fakeProvider{func(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls.Add(1)
				if ctx.Value(testKey{}) != "identity" {
					return llm.Message{}, "", llm.Usage{}, errors.New("lost host context")
				}
				if len(r.Tools) != 0 || len(r.System) != 2 || r.DynamicBoundary != 1 || r.MaxTokens != 777 {
					return llm.Message{}, "", llm.Usage{}, fmt.Errorf("options lost: %+v", r)
				}
				return textResponse("hello child")
			}}
			s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{}, DisableBackgroundTasks: true, SystemPrompt: []string{"static"}, AppendSystemPrompt: []string{"dynamic"}, DynamicBoundary: 1, MaxTokens: 777, NonStreaming: syncMode})
			if s.PID() == os.Getpid() {
				t.Fatal("in-process session")
			}
			events, err := runTest(t, s, context.WithValue(context.Background(), testKey{}, "identity"), "hello")
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || len(events) == 0 || len(s.Messages()) != 2 {
				t.Fatalf("calls/history/events: %d/%d/%d", calls.Load(), len(s.Messages()), len(events))
			}
		})
	}
}

type testKey struct{}

func TestBashRunsUnderChildAndDefaults(t *testing.T) {
	command := "echo $PPID"
	if runtime.GOOS == "windows" {
		command = `(Get-CimInstance Win32_Process -Filter ("ProcessId=" + $PID)).ParentProcessId`
	}
	bash := tool.NewBash()
	schema := bash.InputSchema()
	schema["properties"].(map[string]any)["command"].(map[string]any)["default"] = command
	decorated := agent.DecorateTool(bash, "decorated bash", schema)
	var calls atomic.Int32
	var parent string
	p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			if !strings.Contains(r.Tools[0].Description, "decorated bash") {
				return llm.Message{}, "", llm.Usage{}, errors.New("description lost")
			}
			return toolsResponse(use("bash1", "Bash", `{"command":""}`))
		}
		for _, m := range r.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolResult {
					for _, c := range b.Content {
						parent += c.Text
					}
				}
			}
		}
		return textResponse("done")
	}}
	s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{decorated}, PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true, WorkingDir: t.TempDir()})
	_, err := runTest(t, s, context.Background(), "pid")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(parent) != strconv.Itoa(s.PID()) {
		t.Fatalf("Bash parent %q, expected child %d (host %d)", parent, s.PID(), os.Getpid())
	}
}

func TestHostToolsIdentityResumeUnlockAndTodo(t *testing.T) {
	unlock := tool.NewUnlockSet()
	todos := tool.NewTodoStore()
	dir := t.TempDir()
	store := transcript.NewStore(dir)
	var invoked atomic.Int32
	host := tool.Build(tool.Spec{Name: "Secret", Schema: objectSchema(), Run: func(ctx context.Context, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
		if ctx.Value(testKey{}) != "owner" || tc.WorkingDir != dir || tc.ToolUseID == "" || tc.MaxOutputChars != 12000 || len(tc.Env) != 1 {
			return tool.Result{}, errors.New("lost host ToolContext/identity")
		}
		if transcript.SessionIDFrom(ctx) != "persist" {
			return tool.Result{}, errors.New("lost transcript attribution")
		}
		invoked.Add(1)
		tc.Emit(tool.ProgressInfo{Message: "host progress"})
		return tool.Text("secret"), nil
	}})
	skill := tool.Build(tool.Spec{Name: "Skill", Schema: objectSchema(), Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		unlock.Add("Secret")
		return tool.Text("unlocked"), nil
	}})
	var calls atomic.Int32
	p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		switch calls.Add(1) {
		case 1:
			return toolsResponse(use("skill", "Skill", `{}`))
		case 2:
			return toolsResponse(use("secret", "ExecuteExtraTool", `{"tool_name":"Secret","params":{}}`))
		case 3:
			return toolsResponse(use("todo", "TodoWrite", `{"todos":[{"content":"shared","status":"in_progress"}]}`))
		default:
			return textResponse("stored")
		}
	}}
	opts := agentcore.Options{Provider: p, Tools: []tool.CoreTool{host, skill}, Todos: todos, DeferredTools: []string{"Secret"}, UnlockSet: unlock, PermissionMode: permission.ModeBypass, Transcript: store, SessionID: "persist", WorkingDir: dir, BashEnv: []string{"TEST_ENV=host"}, MaxToolOutputChars: 12000, DisableBackgroundTasks: true}
	s := startTest(t, opts)
	ctx := context.WithValue(context.Background(), testKey{}, "owner")
	events, err := runTest(t, s, ctx, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if invoked.Load() != 1 || len(todos.List()) != 1 {
		t.Fatalf("unlock/todo: %d %+v", invoked.Load(), todos.List())
	}
	progress := false
	for _, ev := range events {
		if ev.Kind == harness.KindProgress {
			progress = true
		}
		if ev.ToolResult != nil && ev.ToolResult.IsError {
			t.Fatalf("tool failed: %+v", ev.ToolResult)
		}
	}
	if !progress {
		t.Fatal("missing tool progress")
	}
	before := s.Messages()
	_ = s.Close()
	// A fresh factory sees an initially empty unlock set. History reconstruction
	// happens in host code after Resume; Prompt must synchronize that later change.
	opts.UnlockSet = tool.NewUnlockSet()
	opts.Provider = fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if len(r.Messages) < len(before) {
			return llm.Message{}, "", llm.Usage{}, errors.New("history lost")
		}
		if calls.Add(1) == 5 {
			return toolsResponse(use("resumed-secret", "ExecuteExtraTool", `{"tool_name":"Secret","params":{}}`))
		}
		return textResponse("resumed")
	}}
	resumed := startTest(t, opts)
	if err := resumed.Resume("persist"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, resumed.Messages()) {
		t.Fatal("resume snapshot mismatch")
	}
	opts.UnlockSet.Add("Secret")
	_, err = runTest(t, resumed, ctx, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if invoked.Load() != 2 {
		t.Fatalf("resume unlock did not reach child: %d", invoked.Load())
	}
}
func TestErrorIdentityAndDeps(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			sentinel := fmt.Errorf("steering/quota fixture: %w", context.Canceled)
			p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return textResponse("wrong provider")
			}}
			opts := agentcore.Options{Provider: p, Tools: []tool.CoreTool{}, DisableBackgroundTasks: true, NonStreaming: syncMode}
			opts.Deps.CallModel = func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, sentinel) }
			}
			opts.Deps.CallModelSync = func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return llm.Message{}, "", llm.Usage{}, sentinel
			}
			s := startTest(t, opts)
			events, err := runTest(t, s, context.Background(), "error")
			if err != sentinel || !errors.Is(err, context.Canceled) {
				t.Fatalf("error identity lost: %T %v", err, err)
			}
			if len(events) == 0 || events[len(events)-1].Terminal == nil || events[len(events)-1].Terminal.Err != sentinel {
				t.Fatal("terminal error identity lost")
			}
		})
	}
}
func TestBudgetCancelCrash(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			return toolsResponse(use("x", "Unknown", `{}`))
		}}
		s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{}, MaxTurns: 1, DisableBackgroundTasks: true})
		ev, err := runTest(t, s, context.Background(), "budget")
		if err != nil {
			t.Fatal(err)
		}
		if ev[len(ev)-1].Terminal.Reason != harness.ReasonMaxTurns {
			t.Fatalf("budget not preserved: %+v", ev[len(ev)-1])
		}
	})
	for _, name := range []string{"Wait", "Crash"} {
		t.Run(name, func(t *testing.T) {
			hostTool := tool.Build(tool.Spec{Name: name, Schema: objectSchema()})
			p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return toolsResponse(use("x", name, `{}`))
			}}
			config := testConfig()
			config.LocalTool = func(tool.CoreTool) (*LocalToolSpec, error) {
				raw, _ := json.Marshal(name)
				return &LocalToolSpec{Kind: "test", Data: raw}, nil
			}
			s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{hostTool}, PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true}, config)
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			_, err := runTest(t, s, ctx, "end")
			if err == nil {
				t.Fatal("missing cancel/crash error")
			}
			if name == "Wait" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancel error: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-s.process.Peer.Done():
			default:
				t.Fatal("child peer still alive")
			}
		})
	}
}
