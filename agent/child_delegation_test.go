package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// delegationToolNames are the entry points a child conversation must never reach.
var delegationToolNames = []string{"subagents", "subagents_control", "spawn_task"}

// lookalikeTool shares a suffix with a blocked name; filtering is by exact name
// only, so it (and any MCP/custom tool) must stay usable inside children.
const lookalikeTool = "mcp__srv__spawn_task"

// delegationProbe records handler executions and whether the handler's context
// carried the child marker (what host tool guards rely on).
type delegationProbe struct {
	mu    sync.Mutex
	calls map[string]int
	child map[string]bool
}

func (p *delegationProbe) tool(name string) actool.CoreTool {
	return actool.Build(actool.Spec{Name: name, Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls[name]++
		p.child[name] = IsChildChat(ctx)
		return actool.Text("ok"), nil
	}})
}

// installDelegationTools makes every conversation see the delegation tools plus
// a lookalike and an ordinary deferred tool. deferred=true routes the delegation
// tools through the deferred/global-name/unlock wiring (ExecuteExtraTool);
// otherwise their schemas are exposed directly.
func installDelegationTools(t *testing.T, deferred bool) *delegationProbe {
	t.Helper()
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	probe := &delegationProbe{calls: map[string]int{}, child: map[string]bool{}}
	ToolResolve = nil
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		var tools []actool.CoreTool
		for _, name := range append(slices.Clone(delegationToolNames), lookalikeTool, "allowed_deferred") {
			tools = append(tools, probe.tool(name))
		}
		names := []string{"allowed_deferred"}
		if deferred {
			names = append(names, delegationToolNames...)
		}
		return tools, DeferredInfo{Deferred: slices.Clone(names), GlobalNames: slices.Clone(names), Unlock: actool.NewUnlockSet(names...)}, func() {}
	}
	return probe
}

func toolUse(id, name, input string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

// requestToolNames returns the tool schemas the model sees and the names listed
// in the system prompt's <available-deferred-tools> block.
func requestToolNames(req llm.CompletionRequest) (schema, listed []string) {
	for _, tool := range req.Tools {
		schema = append(schema, tool.Name)
	}
	for _, block := range req.System {
		in := false
		for _, line := range strings.Split(block, "\n") {
			switch line = strings.TrimSpace(line); {
			case line == "<available-deferred-tools>":
				in = true
			case line == "</available-deferred-tools>":
				in = false
			case in:
				listed = append(listed, line)
			}
		}
	}
	return schema, listed
}

// runDelegationAttempt has the model try every delegation tool the way it can
// reach them in this mode, plus the lookalike and an ordinary deferred tool.
// It returns tool_result error flags keyed by tool_use id.
func runDelegationAttempt(t *testing.T, ctx context.Context, deferred, child bool) map[string]bool {
	t.Helper()
	var attempts []llm.ContentBlock
	for _, name := range delegationToolNames {
		if deferred || child {
			attempts = append(attempts, toolUse("extra-"+name, actool.ExecuteExtraToolName, `{"tool_name":"`+name+`","params":{}}`))
		}
		if !deferred || child {
			attempts = append(attempts, toolUse("direct-"+name, name, `{}`))
		}
	}
	attempts = append(attempts, toolUse("lookalike", lookalikeTool, `{}`), toolUse("ordinary", actool.ExecuteExtraToolName, `{"tool_name":"allowed_deferred","params":{}}`))
	turn := 0
	var results map[string]bool
	p := controlledProvider{run: func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		turn++
		schema, listed := requestToolNames(req)
		for _, name := range delegationToolNames {
			wantSchema, wantListed := !child && !deferred, !child && deferred
			if slices.Contains(schema, name) != wantSchema || slices.Contains(listed, name) != wantListed {
				t.Errorf("%s exposure wrong (child=%v deferred=%v): schema=%v listed=%v", name, child, deferred, schema, listed)
			}
		}
		if !slices.Contains(schema, lookalikeTool) || !slices.Contains(listed, "allowed_deferred") {
			t.Errorf("unrelated tools were filtered: schema=%v listed=%v", schema, listed)
		}
		if turn == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: attempts}, "tool_use", llm.Usage{}, nil
		}
		results = map[string]bool{}
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolResult {
					results[b.ToolUseID] = b.IsError
				}
			}
		}
		return assistant("done"), "end_turn", llm.Usage{}, nil
	}}
	ca := NewChatAgent(p, "fixture", t.TempDir(), nil, 100000)
	ca.SetNonStreaming(func() bool { return true })
	if _, err := ca.Chat(ctx, "agent", "conv-delegation", "try delegation", 4, time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(results) != len(attempts) {
		t.Fatalf("results=%v for %d attempts", results, len(attempts))
	}
	return results
}

// A child can neither call the delegation tools directly nor smuggle them through
// ExecuteExtraTool, whether they are exposed schemas or deferred names.
func TestChildCannotSpawnOrDelegateDirectlyOrViaExecuteExtraTool(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "schema", true: "deferred"}[deferred], func(t *testing.T) {
			probe := installDelegationTools(t, deferred)
			ctx := WithChatControl(context.Background(), ChatControl{Child: true})
			results := runDelegationAttempt(t, ctx, deferred, true)
			for _, name := range delegationToolNames {
				if !results["direct-"+name] || !results["extra-"+name] || probe.calls[name] != 0 {
					t.Fatalf("%s reachable from child: results=%v calls=%v", name, results, probe.calls)
				}
			}
			if results["lookalike"] || results["ordinary"] || probe.calls[lookalikeTool] != 1 || probe.calls["allowed_deferred"] != 1 {
				t.Fatalf("unrelated tools were blocked: results=%v calls=%v", results, probe.calls)
			}
			if !probe.child[lookalikeTool] {
				t.Fatal("tool handler context lost the child marker")
			}
		})
	}
}

// Ordinary (non-child) conversations keep every delegation tool, and their
// handlers see no child marker so host guards let them through.
func TestMainConversationKeepsDelegationTools(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "schema", true: "deferred"}[deferred], func(t *testing.T) {
			probe := installDelegationTools(t, deferred)
			results := runDelegationAttempt(t, context.Background(), deferred, false)
			for id, failed := range results {
				if failed {
					t.Fatalf("%s failed in main conversation: %v", id, results)
				}
			}
			for _, name := range delegationToolNames {
				if probe.calls[name] != 1 || probe.child[name] {
					t.Fatalf("%s calls=%d child=%v", name, probe.calls[name], probe.child[name])
				}
			}
			if probe.child[lookalikeTool] {
				t.Fatal("main conversation handler saw a child marker")
			}
		})
	}
}

// Schema filtering, deferred/global names and the permission deny rule all come
// from one list, and only exact names are affected.
func TestChildDelegationRulesShareOneList(t *testing.T) {
	probe := &delegationProbe{calls: map[string]int{}, child: map[string]bool{}}
	others := []string{"Bash", lookalikeTool, "spawn_task_status", "allowed_deferred"}
	names := append(slices.Clone(delegationToolNames), others...)
	var tools []actool.CoreTool
	for _, name := range names {
		tools = append(tools, probe.tool(name))
	}
	gotTools, def := childTools(tools, DeferredInfo{Deferred: slices.Clone(names), GlobalNames: slices.Clone(names)})
	var gotNames []string
	for _, tool := range gotTools {
		gotNames = append(gotNames, tool.Name())
	}
	for label, got := range map[string][]string{"tools": gotNames, "deferred": def.Deferred, "global": def.GlobalNames} {
		if !slices.Equal(got, others) {
			t.Fatalf("%s=%v want %v", label, got, others)
		}
	}
	rules := childDisallowedTools()
	for _, name := range names {
		decision := permission.Evaluate(context.Background(), permission.Request{ToolName: name, Ctx: permission.Context{Mode: permission.ModeBypass, Disallowed: rules}})
		if want := slices.Contains(delegationToolNames, name); (decision.Behavior == permission.Deny) != want {
			t.Fatalf("%s decision=%s, denied want %v", name, decision.Behavior, want)
		}
	}
	rules[0] = "mutated"
	if childDisallowedTools()[0] == "mutated" {
		t.Fatal("deny rules share backing storage with the master list")
	}
}
