package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/workflow"
	"github.com/Autumn-27/norma/llm"
)

func linkedTestTask(t *testing.T, s *Server, convID int64) *db.Task {
	t.Helper()
	task, err := s.m.pg.CreateTaskWithOptions("background evidence", "Check the supplied evidence", db.TaskCreateOptions{SourceConversationID: convID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(task.ID) })
	return task
}

func assertRetestStatus(t *testing.T, s *Server, id int64, status string) {
	t.Helper()
	r, err := s.m.pg.FindingRetestForConversation(t.Context(), id)
	if err != nil || r == nil || r.Status != status {
		t.Fatalf("retest=%+v err=%v, want %s", r, err, status)
	}
}

func beginTestRetest(t *testing.T, s *Server, fid int64) *db.Conversation {
	t.Helper()
	_, c, _, err := s.m.pg.CreateFindingRetest(t.Context(), fid, "background lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func runTestRetest(s *Server, c *db.Conversation, msg string) {
	session := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[session] = true
	s.chatMu.Unlock()
	s.runConversation(c, msg, session, false, "")
}

func retestText(text string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{}, nil
}

func TestBackgroundTaskRetestWaitsThroughChatStopAndReturns(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	task := linkedTestTask(t, s, c.ID)
	entered := make(chan struct{})
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-ctx.Done()
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		}
		if n == 2 {
			b, _ := json.Marshal(req.Messages)
			if !strings.Contains(string(b), fmt.Sprintf("task_id=%d", task.ID)) {
				t.Error("parent did not receive its exploration result")
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "background-verdict", Name: "record_finding_retest_result", Input: json.RawMessage(`{"verdict":"fixed","summary":"verified after background completion","evidence":"Controlled test evidence and normal comparison."}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return retestText("BACKGROUND_RETEST_CONFIRMED")
	}})
	runTestRetest(s, c, "first foreground turn")
	awaitDeliveryBoundary(t, entered)
	stop := retestRequest(s.pgStopConversation, http.MethodPost, c.ID, "")
	if stop.Code != http.StatusOK {
		t.Fatal(stop.Code, stop.Body)
	}
	waitRetestIdle(t, s)
	assertRetestStatus(t, s, c.ID, "running")
	live, err := s.m.pg.GetTask(task.ID)
	if err != nil || live == nil || db.IsTerminal(live.Status) || live.Paused {
		t.Fatalf("chat stop changed background task: %+v, %v", live, err)
	}
	res, err := s.findingRetestTools()[1].Call(intercept.WithConvID(t.Context(), c.ID), json.RawMessage(`{"verdict":"fixed","summary":"too soon","evidence":"not finished"}`), nil)
	if err != nil || !res.IsError {
		t.Fatal("accepted a verdict before background completion", res, err)
	}
	if err := s.m.pg.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	// Startup uses this same durable reconciler, including tasks completed while
	// there was no live watcher. No user message is needed to deliver the result.
	s.startTaskDeliveryWorker()
	waitNativeWorkflow(t, func() bool {
		r, _ := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
		return r != nil && r.Status == "completed"
	})
	waitRetestIdle(t, s)
	f, _ := s.m.pg.GetFinding(fid)
	if f.Status != db.FindingFixed || calls.Load() != 3 {
		t.Fatalf("finding=%s calls=%d", f.Status, calls.Load())
	}
	rows, _, err := deliveryActivityList(s.m.pg, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if row.Tool == "spawn_task" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("result cards=%d", count)
	}
	pending, err := s.m.pg.ConversationHasPendingTasks(t.Context(), c.ID, nil)
	if err != nil || pending {
		t.Fatal("result was not acknowledged", pending, err)
	}
}

func TestBackgroundTaskDeliveryQueuesDeduplicatesAndSurvivesReservationStop(t *testing.T) {
	s, _, provider, c := newNativeWorkflowServer(t)
	task := linkedTestTask(t, s, c.ID)
	if err := s.m.pg.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	session := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[session] = true
	s.chatMu.Unlock()
	if err := s.collectTaskDeliveries(); err != nil {
		t.Fatal(err)
	}
	if err := s.collectTaskDeliveries(); err != nil {
		t.Fatal(err)
	}
	s.chatMu.Lock()
	count := len(s.workflowPending[session])
	delete(s.chatBusy, session)
	batch := s.reserveWorkflowDeliveryLocked(session)
	s.chatMu.Unlock()
	if count != 1 || batch == nil {
		t.Fatalf("queued=%d batch=%v", count, batch)
	}
	// Cancel after reservation but before model entry. The batch must be retried
	// once, on a fresh context, without duplicating the durable result card.
	batch.cancel(agent.AbortChatStoppedByUser)
	s.deliverWorkflowBatch(batch)
	waitNativeWorkflow(t, func() bool {
		s.chatMu.Lock()
		defer s.chatMu.Unlock()
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return !s.chatBusy[session] && provider.parentCalls == 1
	})
	rows, _, _ := deliveryActivityList(s.m.pg, c.ID)
	count = 0
	for _, row := range rows {
		if row.Tool == "spawn_task" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate result cards: %d", count)
	}
	if err := s.collectTaskDeliveries(); err != nil {
		t.Fatal(err)
	}
	s.chatMu.Lock()
	queued := len(s.workflowPending[session])
	s.chatMu.Unlock()
	if queued != 0 {
		t.Fatal("acknowledged task was queued again")
	}
}

func TestBackgroundTaskDeletedConversationNeverRevives(t *testing.T) {
	s, _, provider, c := newNativeWorkflowServer(t)
	task := linkedTestTask(t, s, c.ID)
	if err := s.m.pg.DeleteConversation(c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.collectTaskDeliveries(); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.parentCalls != 0 {
		t.Fatal("deleted source conversation received a result")
	}
}

func TestBackgroundRetestWaitsForWorkflowCompletionAndDelivery(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	runCtx, cancel := context.WithCancel(s.ctx)
	release := make(chan struct{})
	manager, err := workflow.New(runCtx, t.TempDir(), 1, func(ctx context.Context, _ workflow.Job) (string, error) {
		select {
		case <-release:
			return "WORKFLOW_EVIDENCE", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, s.workflowCompleted)
	if err != nil {
		t.Fatal(err)
	}
	s.workflows = manager
	t.Cleanup(func() { cancel(); manager.Wait() })
	_, err = manager.Start(s.convBusyKey(c.ID), c.AgentKey, 0, []workflow.Task{{Label: "evidence", Task: "check evidence"}})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		n := calls.Add(1)
		if n == 2 {
			b, _ := json.Marshal(req.Messages)
			if !strings.Contains(string(b), "WORKFLOW_EVIDENCE") {
				t.Error("missing workflow evidence")
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "workflow-verdict", Name: "record_finding_retest_result", Input: json.RawMessage(`{"verdict":"inconclusive","summary":"checked delegated evidence","evidence":"The required target context is unavailable."}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return retestText("WAIT_OR_SUMMARY")
	}})
	runTestRetest(s, c, "delegate and wait")
	waitRetestIdle(t, s)
	assertRetestStatus(t, s, c.ID, "running")
	close(release)
	waitNativeWorkflow(t, func() bool {
		r, _ := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
		return r != nil && r.Status == "completed"
	})
	waitRetestIdle(t, s)
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestBackgroundRetestWaitsForEveryResultBatch(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	first, second := linkedTestTask(t, s, c.ID), linkedTestTask(t, s, c.ID)
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		n := calls.Add(1)
		if n == 2 || n == 4 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprintf("verdict-%d", n), Name: "record_finding_retest_result", Input: json.RawMessage(`{"verdict":"inconclusive","summary":"review evidence","evidence":"Partial fixture evidence."}`)}}}, "tool_use", llm.Usage{}, nil
		}
		if n == 3 {
			b, _ := json.Marshal(req.Messages)
			if !strings.Contains(string(b), "尚未处理完") {
				t.Error("premature verdict was not rejected")
			}
		}
		return retestText("WAIT_OR_FINAL")
	}})
	runTestRetest(s, c, "wait for two background results")
	waitRetestIdle(t, s)
	for index, task := range []*db.Task{first, second} {
		if err := s.m.pg.SetStatus(task.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if err := s.collectTaskDeliveries(); err != nil {
			t.Fatal(err)
		}
		waitRetestIdle(t, s)
		if index == 0 {
			assertRetestStatus(t, s, c.ID, "running")
		}
	}
	assertRetestStatus(t, s, c.ID, "completed")
	if calls.Load() != 5 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestBackgroundRetestOnlyParentMaySaveAndCannotDelegateAfterVerdict(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	r, _ := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
	if _, err := s.m.pg.StartFindingRetest(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	parent := intercept.WithConvID(t.Context(), c.ID)
	input := json.RawMessage(`{"verdict":"inconclusive","summary":"reviewed","evidence":"Actual fixture evidence."}`)
	child := agent.WithChatControl(parent, agent.ChatControl{Child: true})
	result, err := s.findingRetestTools()[1].Call(child, input, nil)
	if err != nil || !result.IsError {
		t.Fatal("child saved the parent's verdict", err)
	}
	result, err = s.findingRetestTools()[1].Call(parent, input, nil)
	if err != nil || result.IsError {
		t.Fatal("parent could not save verdict", result.Flatten(), err)
	}
	if err := s.checkRetestDelegation(parent, c.ID); err == nil {
		t.Fatal("new delegation accepted after final verdict")
	}
}

func TestBackgroundRetestRecoveryKeepsLinkedTasks(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	task := linkedTestTask(t, s, c.ID)
	r, _ := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
	if _, err := s.m.pg.StartFindingRetest(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.RecoverFindingRetests(); err != nil {
		t.Fatal(err)
	}
	assertRetestStatus(t, s, c.ID, "running")
	if err := s.m.pg.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.RecoverFindingRetests(); err != nil {
		t.Fatal(err)
	}
	assertRetestStatus(t, s, c.ID, "running") // The deletion result must still be consumed.
}

func TestBackgroundSpawnLinksSourceBeforeQueuedExecution(t *testing.T) {
	s, _, _, c := newNativeWorkflowServer(t)
	s.m.tasks = make(map[string]*Task)
	s.engine = NewEngine(s.m)
	if err := s.m.SetConcurrency(true, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.SetConcurrency(false, 5) })
	ctx := agent.WithRunInfo(intercept.WithConvID(s.ctx, c.ID), agent.RunInfo{SessionID: s.convBusyKey(c.ID)})
	result, err := s.toolSpawnTask().Call(ctx, json.RawMessage(`{"description":"queued from conversation","goal":"analyze supplied material"}`), nil)
	if err != nil || result.IsError {
		t.Fatal(result.Flatten(), err)
	}
	var output struct {
		TaskID  string `json:"task_id"`
		Returns bool   `json:"returns_to_conversation"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &output); err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(output.TaskID, 10, 64)
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(id) })
	var owner int64
	if err := s.m.pg.QueryRow(`SELECT conversation_id FROM conversation_tasks WHERE task_id=$1`, id).Scan(&owner); err != nil || owner != c.ID || !output.Returns {
		t.Fatalf("source=%d output=%+v err=%v", owner, output, err)
	}
	row, _ := s.m.pg.GetTask(id)
	if !row.Queued {
		t.Fatal("fixture unexpectedly started an exploration model")
	}
	before := len(s.m.List())
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result, err = s.toolSpawnTask().Call(cancelled, json.RawMessage(`{"goal":"must not create"}`), nil)
	if err != nil || !result.IsError || len(s.m.List()) != before {
		t.Fatal("cancelled spawn created a task", err)
	}
}
