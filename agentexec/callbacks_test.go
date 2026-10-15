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

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type testHooks struct {
	pre, post, stop, start, prompt atomic.Int32
	identity                       atomic.Bool
}

func (h *testHooks) check(ctx context.Context) {
	if ctx.Value(testKey{}) != "hooks" {
		h.identity.Store(true)
	}
}
func (h *testHooks) PreToolUse(ctx context.Context, _ string, _ []byte) (bool, string, []byte) {
	h.check(ctx)
	h.pre.Add(1)
	return false, "", []byte(`{"value":"updated"}`)
}
func (h *testHooks) PostToolUse(ctx context.Context, _ string, _, _ []byte, _ bool) {
	h.check(ctx)
	h.post.Add(1)
}
func (h *testHooks) Stop(ctx context.Context, _ []llm.Message) (bool, []string, string) {
	h.check(ctx)
	h.stop.Add(1)
	return false, nil, ""
}
func (h *testHooks) FireSessionStart(ctx context.Context) { h.check(ctx); h.start.Add(1) }
func (h *testHooks) FireUserPromptSubmit(ctx context.Context, _ string) string {
	h.check(ctx)
	h.prompt.Add(1)
	return "hook context"
}
func TestHooksPermissionAndDeny(t *testing.T) {
	hooks := &testHooks{}
	var approvals, invoked atomic.Int32
	probe := tool.Build(tool.Spec{Name: "Probe", Schema: objectSchema(), Run: func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		if string(in) != `{"value":"updated"}` || ctx.Value(testKey{}) != "hooks" {
			return tool.Result{}, fmt.Errorf("wrong hook input/identity %s", in)
		}
		invoked.Add(1)
		return tool.Text("probe"), nil
	}})
	var requests atomic.Int32
	p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if requests.Add(1) == 1 {
			return toolsResponse(use("p", "Probe", `{}`), use("b", "Bash", `{"command":"must not execute"}`))
		}
		return textResponse("done")
	}}
	s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{probe, tool.NewBash()}, Hooks: hooks, DisallowedTools: []string{"Bash"}, CanUseTool: func(ctx context.Context, name string, _ json.RawMessage, pc permission.Context) permission.Decision {
		if ctx.Value(testKey{}) != "hooks" || name != "Probe" || pc.Mode != permission.ModeDefault {
			return permission.Denied("wrong permission context")
		}
		approvals.Add(1)
		return permission.Allowed()
	}, DisableBackgroundTasks: true})
	events, err := runTest(t, s, context.WithValue(context.Background(), testKey{}, "hooks"), "hooks")
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	for _, ev := range events {
		if ev.ToolResult != nil && ev.ToolResult.ToolUseID == "b" {
			denied = ev.ToolResult.IsError
		}
	}
	if !denied || approvals.Load() != 1 || invoked.Load() != 1 || hooks.pre.Load() != 1 || hooks.post.Load() != 1 || hooks.start.Load() != 1 || hooks.prompt.Load() != 1 || hooks.stop.Load() != 1 || hooks.identity.Load() {
		t.Fatalf("hooks/permissions failed: denied=%v approvals=%d invoked=%d hooks=%+v", denied, approvals.Load(), invoked.Load(), hooks)
	}
	if !strings.Contains(s.Messages()[0].Text(), "hook context") {
		t.Fatal("prompt hook not recorded")
	}
}

type testCompactor struct {
	err                 error
	overflow            atomic.Bool
	pre, reactive, view atomic.Int32
}

func (c *testCompactor) Pre(_ context.Context, m []llm.Message, _ int) []llm.Message {
	c.pre.Add(1)
	return m
}
func (c *testCompactor) Reactive(_ context.Context, m []llm.Message) ([]llm.Message, bool) {
	c.reactive.Add(1)
	return m, true
}
func (c *testCompactor) IsOverflow(err error) bool {
	c.overflow.Store(err == c.err)
	return err == c.err
}
func (c *testCompactor) View(_ context.Context, _ []llm.Message) []llm.Message {
	c.view.Add(1)
	return []llm.Message{llm.UserText("projected")}
}
func TestCompactorContextViewAndRawSnapshot(t *testing.T) {
	compact := &testCompactor{err: errors.New("overflow sentinel")}
	var calls atomic.Int32
	var s *Session
	p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		raw := s.Messages()
		if len(raw) != 1 || raw[0].Text() != "raw user" || r.Messages[len(r.Messages)-1].Text() != "projected" {
			return llm.Message{}, "", llm.Usage{}, errors.New("snapshot/projection ordering lost")
		}
		if calls.Add(1) == 1 {
			return llm.Message{}, "", llm.Usage{}, compact.err
		}
		return textResponse("done")
	}}
	s = startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{}, Compactor: compact, DisableBackgroundTasks: true})
	_, err := runTest(t, s, context.Background(), "raw user")
	if err != nil {
		t.Fatal(err)
	}
	if !compact.overflow.Load() || compact.reactive.Load() != 1 || compact.view.Load() != 2 {
		t.Fatalf("compactor callbacks: %+v", compact)
	}
	if s.Messages()[0].Text() != "raw user" {
		t.Fatal("ContextView changed persistent history")
	}
}
func TestStreamingResponseBoundaryIsOrdered(t *testing.T) {
	var observed atomic.Int32
	var boundary atomic.Bool
	p := fakeProvider{func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return textResponse("unused")
	}}
	opts := agentcore.Options{Provider: p, Tools: []tool.CoreTool{}, DisableBackgroundTasks: true}
	opts.Deps.CallModel = func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "first"}, nil) {
				return
			}
			if observed.Load() != 1 {
				yield(llm.StreamEvent{}, errors.New("provider advanced before event consumer"))
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil) {
				return
			}
			boundary.Store(true)
		}
	}
	s := startTest(t, opts)
	for ev, err := range s.Prompt(context.Background(), "order") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindText {
			if boundary.Load() {
				t.Fatal("response ended before activity")
			}
			observed.Add(1)
		}
		if ev.Terminal != nil && ev.Terminal.Err != nil {
			t.Fatal(ev.Terminal.Err)
		}
	}
	if !boundary.Load() {
		t.Fatal("missing response boundary")
	}
}
func TestNilToolsAndNoa(t *testing.T) {
	var names []string
	p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		for _, t := range r.Tools {
			names = append(names, t.Name)
		}
		return textResponse("done")
	}}
	opts := agentcore.Options{Provider: p, DisableBackgroundTasks: true}
	s := startTest(t, opts)
	if _, err := runTest(t, s, context.Background(), "defaults"); err != nil {
		t.Fatal(err)
	}
	var expected []string
	for _, tool := range tool.DefaultTools() {
		expected = append(expected, tool.Name())
	}
	if !reflect.DeepEqual(names, expected) {
		t.Fatalf("nil defaults %v != %v", names, expected)
	}
	opts.Tools = []tool.CoreTool{}
	if err := noaadapter.Enable(&opts, noaadapter.Options{ArchiveBaseDir: t.TempDir(), SessionID: "noa-fixture"}); err != nil {
		t.Fatal(err)
	}
	names = nil
	s = startTest(t, opts)
	if _, err := runTest(t, s, context.Background(), "noa"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range names {
		if name == "Compress" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Noa tool missing: %v", names)
	}
}

func TestSettlementAndAskUser(t *testing.T) {
	var calls, asked atomic.Int32
	p := fakeProvider{func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			return toolsResponse(use("q", "AskUserQuestion", `{"questions":[{"question":"choose","options":[{"label":"A"},{"label":"B"}]}]}`))
		}
		found := false
		for _, m := range r.Messages {
			if m.Text() == "settlement checkpoint" {
				found = true
			}
		}
		if !found {
			return llm.Message{}, "", llm.Usage{}, errors.New("settlement prompt missing")
		}
		for _, tool := range r.Tools {
			if tool.Name == "Bash" {
				return llm.Message{}, "", llm.Usage{}, errors.New("settlement tool mask lost")
			}
		}
		return textResponse("checkpoint")
	}}
	s := startTest(t, agentcore.Options{Provider: p, Tools: []tool.CoreTool{tool.NewBash()}, MaxTurns: 1, Settlement: &harness.Settlement{Prompt: "settlement checkpoint", MaxTurns: 1, DisabledTools: []string{"Bash"}}, DisableBackgroundTasks: true, AskUser: func(ctx context.Context, q tool.Question) tool.Answer {
		if ctx.Value(testKey{}) == "ask-owner" && q.Question == "choose" {
			asked.Add(1)
		}
		return tool.Answer{Selected: []string{"B"}}
	}})
	events, err := runTest(t, s, context.WithValue(context.Background(), testKey{}, "ask-owner"), "ask")
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1].Terminal
	if asked.Load() != 1 || calls.Load() != 2 || last.Reason != harness.ReasonMaxTurns || last.Text != "checkpoint" {
		t.Fatalf("settlement/AskUser: %+v calls=%d asked=%d", last, calls.Load(), asked.Load())
	}
}
