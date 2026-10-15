package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agentexec"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// Runs before the server TestMain can resolve any application configuration/PG.
func init() {
	if os.Getenv("YETRX_RESOURCE_AGENT_BRIDGE_FIXTURE") != "1" {
		return
	}
	constructors := map[string]agentexec.LocalToolConstructor{}
	for kind, ctor := range WorkerLocalTools() {
		constructors[kind] = ctor
	}
	if err := agentexec.ServeWorker(context.Background(), agentexec.WorkerConfig{LocalTools: constructors}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type resourceFixtureProvider struct{ calls atomic.Int64 }

func (p *resourceFixtureProvider) Complete(_ context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if p.calls.Add(1) == 1 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "fixture-call", Name: "resource_fixture", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("finished")}}, "end_turn", llm.Usage{}, nil
}
func (p *resourceFixtureProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, fmt.Errorf("fixture requires NonStreaming"))
	}
}

func TestResourceActualAgentBridge(t *testing.T) {
	interp, err := exec.LookPath("python")
	if err != nil {
		interp, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python unavailable")
	}
	dir := t.TempDir()
	code := `import os,json,sys
p=json.load(sys.stdin)
print(json.dumps({"value":p["value"],"env":os.environ["RESOURCE_BRIDGE_ENV"],"cwd":os.getcwd(),"parent":os.getppid(),"pid":os.getpid()}))`
	raw, _ := json.Marshal(map[string]any{"code": code, "timeout_ms": 5000})
	original := (&Server{}).buildCustomTool(&db.Tool{Key: "resource_fixture", Kind: "script", Exec: raw})
	custom := original.(*preparedCustomTool)
	custom.config.Python = interp
	custom.config.WorkingDir = dir
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "default": "bridge-default"}}}
	var observed atomic.Int64
	resolved := &resourceTestWrapper{CoreTool: agent.DecorateTool(original, "bridge metadata", schema), original: original}
	factory := agentexec.NewFactory(agentexec.Config{Args: []string{"-test.run=^$"}, Env: append(os.Environ(), "YETRX_RESOURCE_AGENT_BRIDGE_FIXTURE=1", "ARTEX_CONFIG=C:/__resourceexec_absent_config__", "ARTEX_PG_DSN="), Limits: execution.Limits{MemoryBytes: 768 << 20, CPUs: 1, Processes: 128},
		LocalTool: func(t actool.CoreTool) (*agentexec.LocalToolSpec, error) {
			kind, data, err := LocalTool(t)
			if err != nil {
				return nil, err
			}
			if kind != "" {
				return &agentexec.LocalToolSpec{Kind: kind, Data: data}, nil
			}
			return agentexec.SDKLocalTool(t)
		},
		OnLocalToolStart: func(_ context.Context, t actool.CoreTool) {
			if t != resolved {
				panic("observer received wrong host identity")
			}
			observed.Add(1)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := factory(ctx, agentcore.Options{Provider: &resourceFixtureProvider{}, Tools: []actool.CoreTool{resolved}, WorkingDir: dir, BashEnv: []string{"RESOURCE_BRIDGE_ENV=bridge-env"}, NonStreaming: true, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	childPID := session.(*agentexec.Session).PID()
	for _, err := range session.Prompt(ctx, "execute controlled fixture") {
		if err != nil {
			t.Fatal(err)
		}
	}
	found := false
	for _, message := range session.Messages() {
		for _, block := range message.Content {
			if block.Type != llm.BlockToolResult {
				continue
			}
			for _, content := range block.Content {
				if !strings.Contains(content.Text, "bridge-default") {
					continue
				}
				var got struct {
					Value, Env, Cwd string
					Parent, PID     int
				}
				if err = json.Unmarshal([]byte(content.Text), &got); err != nil {
					t.Fatal(err)
				}
				if got.Parent != childPID || got.PID == os.Getpid() || got.Env != "bridge-env" || !strings.EqualFold(got.Cwd, dir) {
					t.Fatalf("child-local result %+v agentPID=%d", got, childPID)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("local result missing: %+v", session.Messages())
	}
	if observed.Load() != 1 {
		t.Fatalf("local observation not exactly once: %d", observed.Load())
	}
}
