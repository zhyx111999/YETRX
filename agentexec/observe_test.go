package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestLocalObserverExactlyOnce(t *testing.T) {
	for _, name := range []string{"PID", "Wait", "Crash", "Hang"} {
		t.Run(name, func(t *testing.T) {
			var hostCalls, observed atomic.Int32
			host := tool.Build(tool.Spec{Name: name, Schema: objectSchema(), Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				hostCalls.Add(1)
				return tool.Text("must not run"), nil
			}})
			type observation struct {
				identity any
				original tool.CoreTool
			}
			recorded := make(chan observation, 2)
			config := testConfig()
			config.LocalTool = func(tool.CoreTool) (*LocalToolSpec, error) {
				data, _ := json.Marshal(name)
				return &LocalToolSpec{Kind: "test", Data: data}, nil
			}
			config.OnLocalToolStart = func(ctx context.Context, original tool.CoreTool) {
				observed.Add(1)
				recorded <- observation{ctx.Value(testKey{}), original}
			}
			var calls atomic.Int32
			p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if calls.Add(1) == 1 {
					return toolsResponse(use("x", name, `{}`))
				}
				return textResponse("done")
			}}
			s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{host}, PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true}, config)
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), testKey{}, "meter-owner"), 500*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := runTest(t, s, ctx, "observe")
			_ = s.Close()
			if time.Since(started) > 8*time.Second {
				t.Fatal("Close blocked on tool ignoring cancellation")
			}
			if name == "PID" && err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-recorded:
				if got.identity != "meter-owner" || got.original != host {
					t.Fatalf("observer attribution: %+v", got)
				}

			case <-time.After(3 * time.Second):
				t.Fatal("missing local observation")
			}
			if hostCalls.Load() != 0 || observed.Load() != 1 {
				t.Fatalf("duplicate execution/observation: %d/%d", hostCalls.Load(), observed.Load())
			}
		})
	}
}
func TestCloseDoesNotJoinUncooperativeProvider(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		close(entered)
		<-release
		return textResponse("late")
	}}
	s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{}, DisableBackgroundTasks: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range s.Prompt(context.Background(), "block") {
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Close joined uncooperative provider")
	}
	// Prompt itself remains bound to the host callback's cooperation; once the
	// fake provider is released, it can observe the closed process and finish.
}
func TestToolDeadlinePropagatesToHost(t *testing.T) {
	var bounded atomic.Bool
	probe := tool.Build(tool.Spec{Name: "Deadline", Schema: objectSchema(), Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		_, ok := ctx.Deadline()
		bounded.Store(ok)
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	}})
	var calls atomic.Int32
	p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			return toolsResponse(use("x", "Deadline", `{}`))
		}
		return textResponse("done")
	}}
	s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{probe}, PermissionMode: permission.ModeBypass, MaxDuration: 200 * time.Millisecond, DisableBackgroundTasks: true})
	_, err := runTest(t, s, context.Background(), "deadline")
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !bounded.Load() {
		t.Fatal("host tool lost MaxDuration deadline")
	}
}
