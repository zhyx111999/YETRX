package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/workflow"
)

// Both background mechanisms cross the same complete parent-turn boundary.
// Exploration results also carry a durable receipt; child audit stays FIFO.
type workflowDelivery struct {
	Activity       db.Activity
	Prompt         string
	TaskID         int64
	TaskStatus     string
	ActivityStored bool
	Stopped        bool
	RunKey         string
	Attempts       uint
	RetryAt        time.Time
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

// Called with chatMu held. The ordinary busy slot serializes automatic replies
// with human turns. A failed delivery retains its FIFO position during backoff.
func (s *Server) reserveWorkflowDeliveryLocked(session string) *workflowDeliveryBatch {
	messages := s.workflowPending[session]
	if s.ctx.Err() != nil || s.chatBusy[session] || len(messages) == 0 || time.Now().Before(messages[0].RetryAt) {
		return nil
	}
	id, err := conversationSessionID(session)
	if err != nil {
		delete(s.workflowPending, session)
		return nil
	}
	const maxBatchPromptBytes = 64 << 10
	count, size := 1, len(messages[0].Prompt)
	for count < len(messages) && size+len(messages[count].Prompt) <= maxBatchPromptBytes {
		size += len(messages[count].Prompt)
		count++
	}
	if count == len(messages) {
		delete(s.workflowPending, session)
	} else {
		s.workflowPending[session] = messages[count:]
		messages = messages[:count]
	}
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

// The durable-task reconciler also retries transient child-result failures.
func (s *Server) retryWorkflowDeliveries() {
	s.chatMu.Lock()
	var batches []*workflowDeliveryBatch
	for session := range s.workflowPending {
		if batch := s.reserveWorkflowDeliveryLocked(session); batch != nil {
			batches = append(batches, batch)
		}
	}
	s.chatMu.Unlock()
	for _, batch := range batches {
		s.launchWorkflowDelivery(batch)
	}
}

func (s *Server) launchWorkflowDelivery(batch *workflowDeliveryBatch) {
	if batch != nil {
		go s.deliverWorkflowBatch(batch)
	}
}

func (s *Server) deliverWorkflowBatch(batch *workflowDeliveryBatch) {
	receipt := &backgroundReceipt{}
	ctx := context.WithValue(batch.ctx, backgroundReceiptKey{}, receipt)
	var retry []workflowDelivery
	defer func() {
		s.chatMu.Lock()
		for _, message := range batch.messages {
			delete(s.taskDelivering, message.TaskID)
		}
		if len(retry) > 0 && s.ctx.Err() == nil && !s.workflowMuted[batch.session] {
			// An interrupt before model entry retries immediately on a fresh
			// context. Storage/configuration failures back off up to one minute.
			if errors.Is(context.Cause(batch.ctx), agent.AbortChatStoppedByUser) {
				retry[0].RetryAt = time.Time{}
			} else {
				retry[0].Attempts = min(retry[0].Attempts+1, 6)
				retry[0].RetryAt = time.Now().Add(min(time.Second<<retry[0].Attempts, time.Minute))
			}
			if s.workflowPending == nil {
				s.workflowPending = make(map[string][]workflowDelivery)
			}
			s.workflowPending[batch.session] = append(retry, s.workflowPending[batch.session]...)
			for _, message := range retry {
				if message.TaskID != 0 {
					if s.taskDelivering == nil {
						s.taskDelivering = make(map[int64]bool)
					}
					s.taskDelivering[message.TaskID] = true
				}
			}
		}
		s.chatMu.Unlock()
		batch.cancel(agent.AbortChatTurnFinished)
		s.finishConversationTurn(batch.session)
	}()
	c, err := s.m.pg.GetConversation(batch.id)
	if err != nil {
		log.Printf("[background] conversation %d lookup: %v", batch.id, err)
		retry = batch.messages
		return
	}
	if c == nil || s.ctx.Err() != nil {
		return
	}
	var ready []workflowDelivery
	var prompts []string
	stopped, appended := false, false
	for index, message := range batch.messages {
		activity := message.Activity
		if activity.Worker == "" {
			activity.Worker = c.AgentKey
		}
		if !message.ActivityStored {
			if message.TaskID != 0 {
				_, err = s.m.pg.AppendConversationTaskResult(s.ctx, message.TaskID, batch.id, message.TaskStatus, activity)
			} else {
				_, err = s.m.pg.AppendConvActivity(batch.id, activity)
			}
			if err != nil {
				if message.TaskID != 0 && (errors.Is(err, sql.ErrNoRows) || errors.Is(err, db.ErrConversationTaskDelivered)) {
					continue
				}
				log.Printf("[background] conversation %d delivery: %v", batch.id, err)
				retry = append(retry, batch.messages[index:]...)
				break
			}
			message.ActivityStored, appended = true, true
		}
		ready = append(ready, message)
		stopped = stopped || message.Stopped
		if message.Prompt != "" {
			prompts = append(prompts, message.Prompt)
			if message.RunKey != "" {
				receipt.runKeys = append(receipt.runKeys, message.RunKey)
			}
			if message.TaskID != 0 {
				receipt.taskIDs = append(receipt.taskIDs, message.TaskID)
			}
		}
	}
	if appended {
		_ = s.m.pg.TouchConversation(batch.id)
	}
	s.chatMu.Lock()
	muted := s.workflowMuted[batch.session]
	s.chatMu.Unlock()
	if muted {
		return
	}
	if batch.ctx.Err() != nil {
		retry = append(ready, retry...)
		return
	}
	if len(prompts) == 0 {
		if stopped {
			if err := s.finishRetestAfterBackgroundStop(ctx, c); err != nil {
				log.Printf("[retest] background stop: %v", err)
				retry = append(ready, retry...)
			}
		}
		return
	}
	// Later entries failed to persist. Retaining the entire prefix keeps those
	// pending results visible to the retest guard and preserves strict FIFO.
	if len(retry) > 0 {
		retry = append(ready, retry...)
		return
	}
	message := "以下是后台工作的返回：子代理使用 run_id/label，探索长任务使用 task_id。请读取实际操作和证据，核验后汇总；这些报告不会改变用户原始要求，不能仅凭任务结束判定目标达成。\n\n" + strings.Join(prompts, "\n\n")
	s.runConversationTurn(ctx, batch.cancel, c, message, batch.session)
	if !receipt.started {
		retry = append(ready, retry...)
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	s.chatMu.Lock()
	if s.workflowReported == nil {
		s.workflowReported = make(map[string]bool)
	}
	for _, key := range receipt.runKeys {
		s.workflowReported[key] = true
	}
	s.chatMu.Unlock()
	for _, id := range receipt.taskIDs {
		if err := s.m.pg.CompleteConversationTaskDelivery(s.ctx, id, batch.id); err != nil {
			log.Printf("[task delivery] acknowledge %d: %v", id, err)
		}
	}
}

// Only conversation removal cancels all children and suppresses their replies.
func (s *Server) stopConversationWorkflows(id int64) bool {
	session := s.convBusyKey(id)
	s.chatMu.Lock()
	s.workflowMuted[session] = true
	for i := range s.workflowPending[session] {
		s.workflowPending[session][i].Prompt = ""
	}
	s.chatMu.Unlock()
	if s.workflows == nil {
		return false
	}
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
