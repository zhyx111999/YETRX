package server

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
	"github.com/Autumn-27/norma/llm"
)

func deliveryActivityList(pg *db.DB, conversationID int64) ([]db.Activity, int64, error) {
	rows, cursor, err := pg.ConvActivityList(conversationID, 0, 1000)
	if err != nil {
		return nil, cursor, err
	}
	for i := range rows {
		rows[i].Detail, err = pg.ConvActivityDetail(conversationID, rows[i].ID)
		if err != nil {
			return nil, cursor, err
		}
	}
	return rows, cursor, nil
}

type deliveryBoundaryProvider struct {
	mu       sync.Mutex
	requests []string
	started  [2]chan struct{}
	release  [2]chan struct{}
}

func (p *deliveryBoundaryProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	var latest string
	for _, message := range req.Messages {
		if message.Role == llm.RoleUser {
			latest = message.Text()
		}
	}
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, latest)
	p.mu.Unlock()
	if index < len(p.started) {
		close(p.started[index])
		select {
		case <-p.release[index]:
		case <-ctx.Done():
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(fmt.Sprintf("PARENT_OUTPUT_%d", index+1))}}, "end_turn", llm.Usage{}, nil
}

func (p *deliveryBoundaryProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "Streaming parent: "}, nil) {
			return
		}
		message, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: message.Text()}, nil) {
			return
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func awaitDeliveryBoundary(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("parent did not reach the expected reply boundary")
	}
}

func TestWorkflowDeliveryWaitsForParentOutputAndFlushesImmediately(t *testing.T) {
	for _, nonStreaming := range []bool{true, false} {
		t.Run(fmt.Sprintf("nonstream=%t", nonStreaming), func(t *testing.T) {
			s, e, _, conv := newNativeWorkflowServer(t)
			provider := &deliveryBoundaryProvider{}
			for i := range provider.started {
				provider.started[i], provider.release[i] = make(chan struct{}), make(chan struct{})
			}
			chat := agent.NewChatAgent(provider, "fixture", t.TempDir(), nil, 100000)
			chat.SetNonStreaming(func() bool { return nonStreaming })
			s.chatAgent = chat
			session := s.convBusyKey(conv.ID)
			s.chatMu.Lock()
			s.chatBusy[session] = true
			s.chatMu.Unlock()
			s.runConversation(conv, "ORIGINAL_PARENT_REQUEST", session, false, "")
			awaitDeliveryBoundary(t, provider.started[0])
			s.queueWorkflowDelivery(session, workflowDelivery{Activity: db.Activity{Worker: "subagent:first", Kind: "text", Summary: "CHILD_AUDIT", Detail: "CHILD_AUDIT"}})
			s.workflowCompleted(workflow.Run{ID: "wf-first", OwnerSession: session, Status: workflow.Succeeded, Tasks: []workflow.Task{{Label: "one", Result: "FIRST_CHILD"}}})
			s.workflowCompleted(workflow.Run{ID: "wf-second", OwnerSession: session, Status: workflow.Paused, Tasks: []workflow.Task{{Label: "two", Result: "SECOND_CHILD"}}})
			rows, _, err := deliveryActivityList(e.pg, conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Tool == "subagents" || strings.HasPrefix(row.Worker, "subagent:") {
					t.Fatal("child output appeared during parent reply")
				}
			}
			close(provider.release[0])
			// No poll, user message, or explicit queue pump: the parent defer must
			// reserve and start this follow-up as soon as its output is complete.
			awaitDeliveryBoundary(t, provider.started[1])
			s.workflowCompleted(workflow.Run{ID: "wf-third", OwnerSession: session, Status: workflow.Failed, Tasks: []workflow.Task{{Label: "three", Result: "THIRD_CHILD"}}})
			rows, _, err = deliveryActivityList(e.pg, conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Tool == "subagents" && strings.Contains(row.Detail, "wf-third") {
					t.Fatal("next result interrupted automatic parent reply")
				}
			}
			close(provider.release[1])
			waitNativeWorkflow(t, func() bool {
				s.chatMu.Lock()
				defer s.chatMu.Unlock()
				provider.mu.Lock()
				defer provider.mu.Unlock()
				return !s.chatBusy[session] && len(provider.requests) == 3
			})
			provider.mu.Lock()
			requests := append([]string(nil), provider.requests...)
			provider.mu.Unlock()
			if requests[0] != "ORIGINAL_PARENT_REQUEST" || strings.Index(requests[1], "wf-first") < 0 || strings.Index(requests[1], "wf-first") >= strings.Index(requests[1], "wf-second") || !strings.Contains(requests[2], "wf-third") {
				t.Fatalf("wrong FIFO/model prompts: %v", requests)
			}
			rows, _, err = deliveryActivityList(e.pg, conv.ID)
			if err != nil {
				t.Fatal(err)
			}
			position := map[string]int{}
			for i, row := range rows {
				for _, marker := range []string{"PARENT_OUTPUT_1", "CHILD_AUDIT", "wf-first", "wf-second", "PARENT_OUTPUT_2", "wf-third", "PARENT_OUTPUT_3"} {
					if _, exists := position[marker]; !exists && strings.Contains(row.Detail+row.Summary, marker) {
						position[marker] = i
					}
				}
			}
			previous := -1
			for _, marker := range []string{"PARENT_OUTPUT_1", "CHILD_AUDIT", "wf-first", "wf-second", "PARENT_OUTPUT_2", "wf-third", "PARENT_OUTPUT_3"} {
				index, ok := position[marker]
				if !ok || index <= previous {
					t.Fatalf("interleaved/missing %s: %v", marker, position)
				}
				previous = index
			}
		})
	}
}

// stopConversationWorkflows is the deletion path (the chat stop button does not
// use it): it mutes automatic replies but keeps the queued audit.
func TestWorkflowDeliveryQueuesOnlyItsOwnConversationAndDeletionStopKeepsAudit(t *testing.T) {
	s, e, provider, first := newNativeWorkflowServer(t)
	second, err := e.pg.CreateConversation("agent", "independent delivery", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteConversation(second.ID) })
	a, b := s.convBusyKey(first.ID), s.convBusyKey(second.ID)
	s.chatMu.Lock()
	s.chatBusy[a] = true
	s.chatMu.Unlock()
	for _, session := range []string{a, b} {
		s.workflowCompleted(workflow.Run{ID: "wf-" + session, OwnerSession: session, Status: workflow.Succeeded})
	}
	waitNativeWorkflow(t, func() bool {
		s.chatMu.Lock()
		defer s.chatMu.Unlock()
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return !s.chatBusy[b] && provider.parentCalls == 1
	})
	rows, _, err := e.pg.ConvActivityList(first.ID, 0, 100)
	if err != nil || len(rows) != 0 {
		t.Fatal("busy conversation received another conversation's result", err)
	}
	s.stopConversationWorkflows(first.ID)
	s.finishConversationTurn(a)
	waitNativeWorkflow(t, func() bool { s.chatMu.Lock(); defer s.chatMu.Unlock(); return !s.chatBusy[a] })
	rows, _, err = e.pg.ConvActivityList(first.ID, 0, 100)
	if err != nil || len(rows) != 1 || rows[0].Tool != "subagents" {
		t.Fatalf("deletion stop lost queued audit: %+v, %v", rows, err)
	}
	provider.mu.Lock()
	calls := provider.parentCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatal("muted conversation restarted the parent")
	}
}

func TestWorkflowDeliveryDeletedConversationDoesNotRestart(t *testing.T) {
	s, e, provider, conv := newNativeWorkflowServer(t)
	session := s.convBusyKey(conv.ID)
	s.chatMu.Lock()
	s.chatBusy[session] = true
	s.chatMu.Unlock()
	s.workflowCompleted(workflow.Run{ID: "wf-deleted", OwnerSession: session, Status: workflow.Succeeded})
	if err := e.pg.DeleteConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	s.finishConversationTurn(session)
	waitNativeWorkflow(t, func() bool { s.chatMu.Lock(); defer s.chatMu.Unlock(); return !s.chatBusy[session] })
	provider.mu.Lock()
	calls := provider.parentCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatal("deleted conversation restarted")
	}
}
