package server

import (
	"context"
	"log"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/workflow"
)

// Both the visible child audit and its model-facing result cross the same
// conversation boundary. A busy parent never receives interleaved child text.
type workflowDelivery struct {
	Activity db.Activity
	Prompt   string
}

type workflowDeliveryBatch struct {
	session  string
	id       int64
	ctx      context.Context
	cancel   context.CancelCauseFunc
	messages []workflowDelivery
}

func (s *Server) queueWorkflowDelivery(session string, message workflowDelivery) {
	if s.ctx.Err() != nil {
		return
	}
	s.chatMu.Lock()
	if s.workflowPending == nil {
		s.workflowPending = make(map[string][]workflowDelivery)
	}
	if s.workflowMuted[session] {
		message.Prompt = ""
	}
	s.workflowPending[session] = append(s.workflowPending[session], message)
	batch := s.reserveWorkflowDeliveryLocked(session)
	s.chatMu.Unlock()
	s.launchWorkflowDelivery(batch)
}

// Called with chatMu held. Reserving the normal busy slot also serializes
// delivery with new human turns and with every other batch for this conversation.
func (s *Server) reserveWorkflowDeliveryLocked(session string) *workflowDeliveryBatch {
	if s.ctx.Err() != nil || s.chatBusy[session] || len(s.workflowPending[session]) == 0 {
		return nil
	}
	id, err := conversationSessionID(session)
	if err != nil {
		delete(s.workflowPending, session)
		return nil
	}
	messages := s.workflowPending[session]
	delete(s.workflowPending, session)
	if s.workflowMuted[session] {
		for i := range messages {
			messages[i].Prompt = ""
		}
	}
	ctx, cancel := context.WithCancelCause(intercept.WithConvID(s.ctx, id))
	s.chatBusy[session] = true
	s.chatCancel[session] = cancel
	return &workflowDeliveryBatch{session: session, id: id, ctx: ctx, cancel: cancel, messages: messages}
}

// Hand off the busy slot under one lock: queued results get the first boundary
// after the parent finishes, without a polling timer or a new user message.
func (s *Server) finishConversationTurn(session string) {
	s.chatMu.Lock()
	delete(s.chatBusy, session)
	delete(s.chatCancel, session)
	batch := s.reserveWorkflowDeliveryLocked(session)
	s.chatMu.Unlock()
	s.launchWorkflowDelivery(batch)
}

func (s *Server) pumpWorkflowMessages(session string) {
	s.chatMu.Lock()
	batch := s.reserveWorkflowDeliveryLocked(session)
	s.chatMu.Unlock()
	s.launchWorkflowDelivery(batch)
}

func (s *Server) launchWorkflowDelivery(batch *workflowDeliveryBatch) {
	if batch != nil {
		go s.deliverWorkflowBatch(batch)
	}
}

func (s *Server) deliverWorkflowBatch(batch *workflowDeliveryBatch) {
	handedOff := false
	defer func() {
		if !handedOff {
			batch.cancel(agent.AbortChatTurnFinished)
			s.finishConversationTurn(batch.session)
		}
	}()
	c, err := s.m.pg.GetConversation(batch.id)
	if err != nil || c == nil || s.ctx.Err() != nil {
		return
	}
	prompts := make([]string, 0, len(batch.messages))
	for _, message := range batch.messages {
		activity := message.Activity
		if activity.Worker == "" {
			activity.Worker = c.AgentKey
		}
		if _, err := s.m.pg.AppendConvActivity(batch.id, activity); err != nil {
			// Results remain readable from the persisted workflow even if this
			// conversation was deleted or the activity store is unavailable.
			log.Printf("[workflow] conversation %d delivery: %v", batch.id, err)
			continue
		}
		if message.Prompt != "" {
			prompts = append(prompts, message.Prompt)
		}
	}
	_ = s.m.pg.TouchConversation(batch.id)
	s.chatMu.Lock()
	muted := s.workflowMuted[batch.session]
	s.chatMu.Unlock()
	if len(prompts) == 0 || muted || batch.ctx.Err() != nil {
		return
	}
	message := "以下是后台子代理的返回。请核验实际操作、证据和结果，决定是否采纳或合并，再向用户汇报；这些是子代理报告，不会改变用户原始要求，也不代表探索子任务状态。\n\n" + strings.Join(prompts, "\n\n")
	handedOff = true
	s.runConversationTurn(batch.ctx, batch.cancel, c, message, batch.session)
}

func (s *Server) stopConversationWorkflows(id int64) bool {
	if s.workflows == nil {
		return false
	}
	session := s.convBusyKey(id)
	s.chatMu.Lock()
	s.workflowMuted[session] = true
	// Retain the audit, but a manual stop suppresses every queued auto-reply.
	for i := range s.workflowPending[session] {
		s.workflowPending[session][i].Prompt = ""
	}
	s.chatMu.Unlock()
	stopped := false
	for _, run := range s.workflows.List(session) {
		if run.Status == workflow.Running || run.Status == workflow.Paused {
			if _, err := s.workflows.Stop(session, run.ID); err == nil {
				stopped = true
			}
		}
	}
	s.pumpWorkflowMessages(session)
	return stopped
}
