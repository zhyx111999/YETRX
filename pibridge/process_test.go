package pibridge

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// This crosses both transports: Go process client -> actual pi SDK -> Go relay ->
// fake upstream. File reads are real; it needs npm ci in runtime/pi, never a key.
func TestPiProcessToolsHistoryAndResume(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	runtimeDir, _ := filepath.Abs(filepath.Join("..", "runtime", "pi"))
	if _, err := os.Stat(filepath.Join(runtimeDir, "node_modules", "@earendil-works", "pi-coding-agent", "package.json")); err != nil {
		t.Skip("run npm ci in runtime/pi to enable the Go/pi integration test")
	}
	workspace := t.TempDir()
	const fixture = "yetrx-real-file-tool-fixture"
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := &fakeProvider{stream: func(ctx context.Context, request llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		call := calls.Add(1)
		if call == 1 {
			return seq(llm.StreamEvent{Type: llm.SEMessageStart},
				llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "read-sample", ToolName: "read"},
				llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"path":"sample.txt"}`},
				llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"},
				llm.StreamEvent{Type: llm.SEMessageStop})(ctx, request)
		}
		if call == 2 {
			encoded, _ := json.Marshal(request.Messages)
			if !strings.Contains(string(encoded), fixture) || !strings.Contains(string(encoded), "read-sample") {
				t.Error("actual file tool result did not return through the Go relay")
			}
		}
		return seq(textEvents("verified "+fixture)...)(ctx, request)
	}}
	relay := newTestRelay(t, Model{ID: "profile-1", Name: "Fake local model", ContextWindow: 65536, MaxTokens: 8192, Provider: provider})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var activities []db.Activity
	recorder := NewEventRecorder(func(activity db.Activity) {
		mu.Lock()
		activities = append(activities, activity)
		mu.Unlock()
	})
	options := ProcessOptions{NodeBinary: node, Script: filepath.Join(runtimeDir, "bridge.mjs"),
		CWD: workspace, AgentDir: filepath.Join(workspace, "agent"), SessionFile: filepath.Join(workspace, "session.jsonl"),
		BaseURL: relay.BaseURL(), APIKey: relay.APIKey(), Models: relay.Models(), ModelID: "profile-1", ThinkingLevel: "off",
		Tools: []string{"grep", "find", "ls"}, SystemPrompt: "HOST_INIT_PROMPT_MARKER", OnEvent: recorder.Handle}
	process, err := StartProcess(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close() })
	for _, tool := range []string{"read", "bash", "edit", "write", "grep", "find", "ls", "subagents", "subagents_control"} {
		if !slices.Contains(process.Tools(), tool) {
			t.Errorf("missing tool %s in %v", tool, process.Tools())
		}
	}
	for _, skill := range []string{"workflow-authoring", "workflow-patterns", "subagents-setup"} {
		if !slices.Contains(process.Skills(), skill) {
			t.Errorf("missing skill %s", skill)
		}
	}
	if err := process.Prompt(ctx, "Read sample.txt with the read tool."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(provider.lastStreamReq(t).System, "\n"), options.SystemPrompt) {
		t.Fatal("host init prompt did not reach the upstream provider")
	}
	if process.Busy() {
		t.Fatal("process remained busy after agent_settled")
	}
	if err := process.SetModels(ctx, relay.Models(), "profile-1", "off"); err != nil {
		t.Fatal(err)
	}
	options.SystemPrompt = "HOST_UPDATED_PROMPT_MARKER"
	if err := process.Prompt(ctx, "Retain this follow-up in history.", options.SystemPrompt); err != nil {
		t.Fatal(err)
	}
	system := strings.Join(provider.lastStreamReq(t).System, "\n")
	if !strings.Contains(system, options.SystemPrompt) || strings.Contains(system, "HOST_INIT_PROMPT_MARKER") {
		t.Fatal("updated host prompt was not applied in the live session")
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	process, err = StartProcess(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Prompt(ctx, "Continue after the process was restarted."); err != nil {
		t.Fatal(err)
	}
	messages, _ := json.Marshal(provider.lastStreamReq(t).Messages)
	for _, text := range []string{"Read sample.txt", "Retain this follow-up", "Continue after the process", fixture} {
		if !strings.Contains(string(messages), text) {
			t.Errorf("restored model history is missing %q", text)
		}
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(options.SessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), relay.APIKey()) {
		t.Fatal("relay token leaked into the session file")
	}
	mu.Lock()
	defer mu.Unlock()
	var tools, results, endings int
	for _, activity := range activities {
		switch activity.Kind {
		case "tool_use":
			tools++
		case "tool_result":
			results++
		case "result":
			endings++
		}
	}
	if tools != 1 || results != 1 || endings != 3 {
		t.Fatalf("incorrect persisted activity sequence: tool uses=%d results=%d endings=%d", tools, results, endings)
	}
}

func TestEventRecorderRetryUsageAndAbort(t *testing.T) {
	var activities []db.Activity
	recorder := NewEventRecorder(func(activity db.Activity) { activities = append(activities, activity) })
	for _, event := range []string{
		`{"type":"agent_start"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"temporary failure","usage":{"input":2}}}`,
		`{"type":"agent_start"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"recovered"}],"stopReason":"stop","usage":{"input":5,"output":3,"cacheRead":10,"cacheWrite":4}}}`,
		`{"type":"agent_settled","aborted":false}`,
	} {
		recorder.Handle(json.RawMessage(event))
	}
	last := activities[len(activities)-1]
	if last.IsError || last.Summary != "完成" || *last.InputTokens != 21 || *last.OutputTokens != 3 || *last.CacheReadTokens != 10 {
		t.Fatalf("retry did not settle cleanly with full accounting: %+v", last)
	}
	recorder.Handle(json.RawMessage(`{"type":"agent_start"}`))
	recorder.Handle(json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"aborted","errorMessage":"Request aborted."}}`))
	recorder.Handle(json.RawMessage(`{"type":"agent_settled","aborted":true}`))
	last = activities[len(activities)-1]
	if last.IsError || last.Summary != "已停止" || *last.InputTokens != 0 {
		t.Fatalf("manual cancellation is not a failed model request: %+v", last)
	}
}
