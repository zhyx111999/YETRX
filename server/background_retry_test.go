package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
	"github.com/Autumn-27/norma/llm"
)

func retryDeliveryNow(s *Server, session string) {
	s.chatMu.Lock()
	if len(s.workflowPending[session]) > 0 {
		s.workflowPending[session][0].RetryAt = time.Time{}
	}
	s.chatMu.Unlock()
	s.retryWorkflowDeliveries()
}

func TestBackgroundDeliveryBoundsBatchWithoutReordering(t *testing.T) {
	s := &Server{ctx: context.Background(), workflowPending: map[string][]workflowDelivery{}}
	session := s.convBusyKey(1)
	for _, key := range []string{"first", "second", "third"} {
		s.workflowPending[session] = append(s.workflowPending[session], workflowDelivery{RunKey: key, Prompt: strings.Repeat("x", 40<<10), ActivityStored: true})
	}
	for _, key := range []string{"first", "second", "third"} {
		batch := s.claimDeliveriesLocked(session)
		if len(batch) != 1 || batch[0].RunKey != key {
			t.Fatalf("bad bounded batch for %s: %+v", key, batch)
		}
	}
	if len(s.workflowPending[session]) != 0 {
		t.Fatal("pending results left after all batches")
	}
}

func TestBackgroundDeliveryClaimsOnlyDisplayedResultsInOrder(t *testing.T) {
	s := &Server{ctx: context.Background(), workflowPending: map[string][]workflowDelivery{}}
	session := s.convBusyKey(1)
	big := strings.Repeat("x", maxBatchPromptBytes+1)
	s.workflowPending[session] = []workflowDelivery{
		{RunKey: "oversized", Prompt: big, ActivityStored: true},
		{RunKey: "audit", ActivityStored: true},
		{RunKey: "shown", Prompt: "shown", ActivityStored: true},
		{RunKey: "hidden", Prompt: "hidden"},
		{RunKey: "after", Prompt: "after", ActivityStored: true},
	}
	if batch := s.claimDeliveriesLocked(session); len(batch) != 1 || batch[0].RunKey != "oversized" {
		t.Fatalf("an oversized first result must travel alone: %+v", batch)
	}
	batch := s.claimDeliveriesLocked(session)
	if len(batch) != 2 || batch[0].RunKey != "audit" || batch[1].RunKey != "shown" {
		t.Fatalf("claim must stop at the first result without a card: %+v", batch)
	}
	if batch := s.claimDeliveriesLocked(session); batch != nil || len(s.workflowPending[session]) != 2 {
		t.Fatalf("an undisplayed result must keep the queue behind it: %+v", batch)
	}
	s.workflowPending[session][0].ActivityStored = true
	s.workflowPending[session][0].RetryAt = time.Now().Add(time.Minute)
	if batch := s.claimDeliveriesLocked(session); batch != nil {
		t.Fatalf("a head in backoff must hold the queue: %+v", batch)
	}
}

func TestBackgroundDeliveryRetainsResultAcrossSetupFailure(t *testing.T) {
	for _, fault := range []string{"no-model", "invalid-workdir"} {
		t.Run(fault, func(t *testing.T) {
			s, fid := newRetestServer(t)
			c := beginTestRetest(t, s, fid)
			task := linkedTestTask(t, s, c.ID)
			var calls atomic.Int32
			provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls.Add(1)
				return retestText("RECOVERED_BACKGROUND_RESULT")
			}}
			blocked := filepath.Join(s.m.dir, "sessions")
			if fault == "invalid-workdir" {
				setRetestProvider(s, provider)
				if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if s.resolveChatAgent(c) != nil {
				t.Fatal("fixture must not have a configured provider")
			}
			if err := s.m.pg.SetStatus(task.ID, "done"); err != nil {
				t.Fatal(err)
			}
			if err := s.collectTaskDeliveries(); err != nil {
				t.Fatal(err)
			}
			waitRetestIdle(t, s)
			assertRetestStatus(t, s, c.ID, "running")
			session := s.convBusyKey(c.ID)
			s.chatMu.Lock()
			pending := s.workflowPending[session]
			s.chatMu.Unlock()
			if len(pending) != 1 || !pending[0].RetryAt.After(time.Now()) || !pending[0].ActivityStored || calls.Load() != 0 {
				t.Fatalf("lost result or no retry backoff: %+v calls=%d", pending, calls.Load())
			}
			for range 3 {
				s.retryWorkflowDeliveries()
				if err := s.collectTaskDeliveries(); err != nil {
					t.Fatal(err)
				}
			}
			rows, _, _ := deliveryActivityList(s.m.pg, c.ID)
			if len(rows) != 2 || rows[0].Kind != "user" || rows[1].Tool != "spawn_task" {
				t.Fatalf("retry generated extra activities: %+v", rows)
			}
			if fault == "invalid-workdir" {
				if err := os.Remove(blocked); err != nil {
					t.Fatal(err)
				}
			}
			setRetestProvider(s, provider)
			retryDeliveryNow(s, session)
			waitRetestIdle(t, s)
			if calls.Load() != 1 {
				t.Fatalf("recovered calls=%d", calls.Load())
			}
			waiting, err := s.m.pg.ConversationHasPendingTasks(t.Context(), c.ID, nil)
			if err != nil || waiting {
				t.Fatal("recovered delivery was not acknowledged", waiting, err)
			}
		})
	}
}

func TestBackgroundWorkflowStartupInterruptionSeesInitializedManager(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	run := workflow.Run{ID: "wf-0123456789abcdef", OwnerSession: s.convBusyKey(c.ID), Status: workflow.Running}
	path := filepath.Join(s.m.dir, "workflows")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(run)
	if err := os.WriteFile(filepath.Join(path, run.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.ctx = ctx
	s.initWorkflows()
	defer func() { cancel(); s.workflows.Wait() }()
	waitNativeWorkflow(t, func() bool {
		r, _ := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
		return r != nil && r.Status == "stopped"
	})
	waitRetestIdle(t, s)
}

func TestBackgroundStoppedNoticeSurvivesReservationCancellation(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	session := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[session] = true
	s.chatMu.Unlock()
	s.queueWorkflowDelivery(session, workflowDelivery{Stopped: true, Activity: db.Activity{Kind: "text", Detail: "explicitly stopped child"}})
	s.chatMu.Lock()
	delete(s.chatBusy, session)
	batch := s.reserveWorkflowDeliveryLocked(session)
	s.chatMu.Unlock()
	s.cancelConversationTurn(c.ID)
	s.deliverWorkflowBatch(batch)
	waitNativeWorkflow(t, func() bool {
		r, _ := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
		return r != nil && r.Status == "stopped"
	})
	waitRetestIdle(t, s)
}

func TestBackgroundDeliveryRetriesFailedChildActivityInOrder(t *testing.T) {
	s, _, provider, c := newNativeWorkflowServer(t)
	_, err := s.m.pg.Exec(`CREATE FUNCTION test_fail_background_activity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.tool='background_retry_fixture' THEN RAISE EXCEPTION 'simulated activity storage failure'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER test_fail_background_activity BEFORE INSERT ON conversation_activities
FOR EACH ROW EXECUTE FUNCTION test_fail_background_activity()`)
	if err != nil {
		t.Fatal(err)
	}
	clear := func() {
		_, _ = s.m.pg.Exec(`DROP TRIGGER IF EXISTS test_fail_background_activity ON conversation_activities; DROP FUNCTION IF EXISTS test_fail_background_activity()`)
	}
	t.Cleanup(clear)
	session := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[session] = true
	s.chatMu.Unlock()
	for _, msg := range []workflowDelivery{
		{Activity: db.Activity{Kind: "text", Tool: "background_retry_fixture", Detail: "FIRST_RESULT"}, Prompt: "FIRST_RESULT", RunKey: "first"},
		{Activity: db.Activity{Kind: "text", Detail: "SECOND_RESULT"}, Prompt: "SECOND_RESULT", RunKey: "second"},
	} {
		s.queueWorkflowDelivery(session, msg)
	}
	s.finishConversationTurn(session)
	waitRetestIdle(t, s)
	s.chatMu.Lock()
	pending := s.workflowPending[session]
	s.chatMu.Unlock()
	if len(pending) != 2 || pending[0].RunKey != "first" || pending[1].RunKey != "second" {
		t.Fatalf("lost/reordered failed batch: %+v", pending)
	}
	clear()
	retryDeliveryNow(s, session)
	waitRetestIdle(t, s)
	provider.mu.Lock()
	calls := provider.parentCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("parent calls=%d", calls)
	}
	rows, _, err := deliveryActivityList(s.m.pg, c.ID)
	if err != nil || len(rows) < 2 || rows[0].Detail != "FIRST_RESULT" || rows[1].Detail != "SECOND_RESULT" {
		t.Fatalf("bad result order: %+v err=%v", rows, err)
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if !s.workflowReported["first"] || !s.workflowReported["second"] {
		t.Fatal("recovered child result still considered pending")
	}
}
