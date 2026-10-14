package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestNativeHistoryImportPreservesLedgerAndRunsOnce(t *testing.T) {
	e := newNativeEnv(t)
	root := t.TempDir()
	s := &Server{m: &Manager{pg: e.pg, dir: root}}
	conv, err := e.pg.CreateConversation("agent", "history upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteConversation(conv.ID) })
	session := s.convBusyKey(conv.ID)
	legacy := filepath.Join(root, "pi", "sessions", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := transcript.NewStore(filepath.Join(root, "transcripts"))
	writer := store.NewWriter(session, "")
	writer.RecordMessage(llm.UserText("old native history"), llm.Usage{})
	old, err := os.ReadFile(store.MainPath(session))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []db.Activity{
		{Kind: "user", Detail: "old request"},
		{Kind: "tool_use", Tool: "write", Detail: `{"path":"result.txt","content":"already written"}`},
		{Kind: "tool_result", Tool: "write", Detail: "write succeeded"},
		{Kind: "text", Detail: "ANSWER_FROM_EMBEDDED_VERSION"},
	} {
		if _, err := e.pg.AppendConvActivity(conv.ID, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ensureNativeHistory(conv.ID); err != nil {
		t.Fatal(err)
	}
	records, err := store.Load(store.MainPath(session))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 || records[0].Type != nativeHistoryMarker {
		t.Fatalf("bad import: %+v", records)
	}
	messages, _ := transcript.Messages(records)
	if messages[len(messages)-1].Text() != "ANSWER_FROM_EMBEDDED_VERSION" {
		t.Fatal("latest answer lost")
	}
	for _, m := range messages {
		if len(m.ToolUses()) != 0 {
			t.Fatal("old tool call would be replayed")
		}
	}
	backup, err := os.ReadFile(store.MainPath(session) + ".before-native")
	if err != nil || string(backup) != string(old) {
		t.Fatal("old transcript backup mismatch", err)
	}
	before, err := os.ReadFile(store.MainPath(session))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.AppendConvActivity(conv.ID, db.Activity{Kind: "user", Detail: "new human turn"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureNativeHistory(conv.ID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.MainPath(session))
	if err != nil || string(before) != string(after) {
		t.Fatal("history imported twice", err)
	}
	rows, _, err := e.pg.ConvActivityList(conv.ID, 0, 20)
	if err != nil || len(rows) != 5 {
		t.Fatal("activity ledger changed", err, len(rows))
	}
}

type nativeWorkflowProvider struct {
	mu                      sync.Mutex
	tool, skill             string
	childCalls, parentCalls int
	childTools              []llm.ToolSchema
}

func (p *nativeWorkflowProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	child, toolResult := false, false
	for _, m := range req.Messages {
		if strings.Contains(m.Text(), "NATIVE_CHILD_REQUEST") {
			child = true
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				toolResult = true
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if child {
		p.childCalls++
		p.childTools = req.Tools
		if !toolResult {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "native-child-call", Name: p.tool, Input: json.RawMessage(`{"msg":"child"}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("NATIVE_CHILD_DONE")}}, "end_turn", llm.Usage{}, nil
	}
	p.parentCalls++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("NATIVE_PARENT_SUMMARY")}}, "end_turn", llm.Usage{}, nil
}
func (p *nativeWorkflowProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, fmt.Errorf("fixture expects non-streaming"))
	}
}

func newNativeWorkflowServer(t *testing.T) (*Server, *nativeEnv, *nativeWorkflowProvider, *db.Conversation) {
	t.Helper()
	e := newNativeEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	provider := &nativeWorkflowProvider{tool: e.custom, skill: e.skill}
	chat := agent.NewChatAgent(provider, "fixture", root, transcript.NewStore(filepath.Join(root, "transcripts")), 100000)
	chat.SetNonStreaming(func() bool { return true })
	s := &Server{ctx: ctx, m: &Manager{pg: e.pg, dir: root}, skillDir: e.skillDir, chatAgent: chat,
		chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}, workflowPending: map[string][]workflowDelivery{}, workflowMuted: map[string]bool{}}
	conv, err := e.pg.CreateConversation("agent", "native workflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.initWorkflows()
	if s.workflows == nil {
		t.Fatal("workflow manager not initialized")
	}
	wireAgentAugment(e.pg, e.skillDir, s.hostTools)
	t.Cleanup(func() { cancel(); s.workflows.Wait(); _ = e.pg.DeleteConversation(conv.ID) })
	return s, e, provider, conv
}

func waitNativeWorkflow(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for native workflow")
}

func TestNativeWorkflowUsesSharedResourcesAndQueuesDelivery(t *testing.T) {
	s, e, provider, conv := newNativeWorkflowServer(t)
	session := s.convBusyKey(conv.ID)
	s.chatBusy[session] = true // foreground owns the transcript until released below
	ctx := agent.WithRunInfo(s.ctx, agent.RunInfo{SessionID: session})
	input, _ := json.Marshal(map[string]any{"tasks": []workflow.Task{{Label: "child", AgentKey: e.keys[1], Task: "NATIVE_CHILD_REQUEST"}}})
	result, err := s.startWorkflow(ctx, input)
	if err != nil || result.IsError {
		t.Fatalf("start: %s %v", result.Flatten(), err)
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &started); err != nil {
		t.Fatal(err)
	}
	waitNativeWorkflow(t, func() bool {
		s.chatMu.Lock()
		defer s.chatMu.Unlock()
		for _, message := range s.workflowPending[session] {
			if message.Prompt != "" {
				return true
			}
		}
		return false
	})
	before, _, err := e.pg.ConvActivityList(conv.ID, 0, 100)
	if err != nil || len(before) != 0 {
		t.Fatalf("child audit/result interleaved with busy parent: %d rows, %v", len(before), err)
	}
	provider.mu.Lock()
	childCalls, parentCalls := provider.childCalls, provider.parentCalls
	tools := append([]llm.ToolSchema(nil), provider.childTools...)
	provider.mu.Unlock()
	if childCalls != 2 || parentCalls != 0 {
		t.Fatalf("child/parent calls: %d/%d", childCalls, parentCalls)
	}
	available := map[string]llm.ToolSchema{}
	for _, tool := range tools {
		available[tool.Name] = tool
	}
	for _, key := range []string{"Read", "Bash", "Skill", e.custom, nativeSystemTool} {
		if _, ok := available[key]; !ok {
			t.Errorf("shared tool absent in child: %s", key)
		}
	}
	if !strings.Contains(available["Skill"].Description, e.skill) {
		t.Fatal("global skill missing from child")
	}
	read, err := s.workflows.Read(session, started.RunID, "child", 8000)
	if err != nil || read != "NATIVE_CHILD_DONE" {
		t.Fatalf("result: %q %v", read, err)
	}
	s.finishConversationTurn(session)
	waitNativeWorkflow(t, func() bool {
		s.chatMu.Lock()
		busy := s.chatBusy[session]
		s.chatMu.Unlock()
		provider.mu.Lock()
		calls := provider.parentCalls
		provider.mu.Unlock()
		return !busy && calls == 1
	})
	rows, _, err := e.pg.ConvActivityList(conv.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if strings.Contains(row.Summary, "NATIVE_PARENT_SUMMARY") {
			found = true
		}
	}
	if !found {
		t.Fatal("automatic parent reply was not persisted")
	}
}

func TestNativeWorkflowStopSuppressesQueuedReplyAndCancelledStart(t *testing.T) {
	s, _, provider, conv := newNativeWorkflowServer(t)
	session := s.convBusyKey(conv.ID)
	s.stopConversationWorkflows(conv.ID)
	s.workflowCompleted(workflow.Run{ID: "fixture", OwnerSession: session, OwnerAgent: "agent", Status: workflow.Succeeded,
		Tasks: []workflow.Task{{Label: "done", Status: workflow.Succeeded, Result: "late completion"}}})
	waitNativeWorkflow(t, func() bool {
		s.chatMu.Lock()
		defer s.chatMu.Unlock()
		return !s.chatBusy[session]
	})
	s.chatMu.Lock()
	pending, busy := len(s.workflowPending[session]), s.chatBusy[session]
	s.chatMu.Unlock()
	if pending != 0 || busy {
		t.Fatal("stopped conversation restarted")
	}
	provider.mu.Lock()
	calls := provider.parentCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatal("model called after stop")
	}
	ctx, cancel := context.WithCancel(agent.WithRunInfo(s.ctx, agent.RunInfo{SessionID: session}))
	cancel()
	result, err := s.startWorkflow(ctx, json.RawMessage(`{"tasks":[{"label":"cancelled","task":"must not run"}]}`))
	if err != nil || !result.IsError || len(s.workflows.List(session)) != 0 {
		t.Fatal("cancelled prompt started a task", err)
	}
}
