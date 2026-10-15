package server

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/workflow"
)

// Results of both background mechanisms (subagent runs, exploration tasks) are
// kept in one FIFO per conversation. A running parent turn takes them at its own
// response boundaries (handoff_turn.go); this file owns the FIFO and the idle
// path, which starts an automatic turn when no turn is running to take them.
// Exploration results also carry a durable receipt; child audit stays FIFO.

// A delivery moves through three states. Queued: in the FIFO, nothing visible.
// Displayed: ActivityStored, the card is in the ledger but the model has not
// read it. Read: handed to the model by OnPrompt and acknowledged when the turn
// ends. Only the last state lets a retest conclude.
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

// Automatic turns carry at most this much result text per user message.
const maxBatchPromptBytes = 64 << 10

const backgroundReturnHeader = "以下是后台工作的返回：子代理使用 run_id/label，探索长任务使用 task_id。请读取实际操作和证据，核验后汇总；这些报告不会改变用户原始要求，不能仅凭任务结束判定目标达成。\n\n"

func backgroundReturnMessage(prompts []string) string {
	return backgroundReturnHeader + strings.Join(prompts, "\n\n")
}

type workflowDeliveryBatch struct {
	session string
	id      int64
	ctx     context.Context
	cancel  context.CancelCauseFunc
	turn    *handoffTurn
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
	running := s.chatTurns[session]
	batch := s.reserveWorkflowDeliveryLocked(session)
	s.chatMu.Unlock()
	s.launchWorkflowDelivery(batch)
	running.showWhenQuiet()
}

// Called with chatMu held. The ordinary busy slot serializes automatic replies
// with human turns. A failed delivery retains its FIFO position during backoff.
func (s *Server) reserveWorkflowDeliveryLocked(session string) *workflowDeliveryBatch {
	queue := s.workflowPending[session]
	if s.ctx.Err() != nil || s.chatBusy[session] || len(queue) == 0 || time.Now().Before(queue[0].RetryAt) {
		return nil
	}
	id, err := conversationSessionID(session)
	if err != nil {
		delete(s.workflowPending, session)
		return nil
	}
	ctx, cancel := context.WithCancelCause(intercept.WithConvID(s.ctx, id))
	s.chatBusy[session] = true
	s.chatCancel[session] = cancel
	return &workflowDeliveryBatch{session: session, id: id, ctx: ctx, cancel: cancel, turn: s.beginTurnLocked(session, id, ctx, nil, true)}
}

// claimDeliveriesLocked removes the next batch from the FIFO head: displayed
// deliveries only, in order, carrying at most maxBatchPromptBytes of result text
// (the first is always taken). A head in backoff keeps everything behind it.
func (s *Server) claimDeliveriesLocked(session string) []workflowDelivery {
	queue := s.workflowPending[session]
	if len(queue) == 0 || time.Now().Before(queue[0].RetryAt) {
		return nil
	}
	count, size := 0, 0
	for count < len(queue) && queue[count].ActivityStored && (count == 0 || size+len(queue[count].Prompt) <= maxBatchPromptBytes) {
		size += len(queue[count].Prompt)
		count++
	}
	if count == 0 {
		return nil
	}
	claimed := append([]workflowDelivery(nil), queue[:count]...)
	if count == len(queue) {
		delete(s.workflowPending, session)
	} else {
		s.workflowPending[session] = queue[count:]
	}
	if s.workflowMuted[session] {
		for i := range claimed {
			claimed[i].Prompt = ""
		}
	}
	return claimed
}

// requeueDeliveriesLocked puts unread deliveries back at the head, keeping their
// FIFO position. An interrupt before the model was reached retries at once; a
// storage or configuration failure backs off up to one minute. Task deliveries
// stay marked as in flight so the collector does not queue them twice.
func (s *Server) requeueDeliveriesLocked(session string, items []workflowDelivery, backoff bool) {
	if len(items) == 0 || s.ctx.Err() != nil || s.workflowMuted[session] {
		s.releaseTasksLocked(items)
		return
	}
	items = append([]workflowDelivery(nil), items...)
	if backoff {
		items[0].Attempts = min(items[0].Attempts+1, 6)
		items[0].RetryAt = time.Now().Add(min(time.Second<<items[0].Attempts, time.Minute))
	} else {
		items[0].RetryAt = time.Time{}
	}
	if s.workflowPending == nil {
		s.workflowPending = make(map[string][]workflowDelivery)
	}
	s.workflowPending[session] = append(items, s.workflowPending[session]...)
	for _, item := range items {
		if item.TaskID != 0 {
			s.markTaskLocked(item.TaskID)
		}
	}
}

func (s *Server) markTaskLocked(id int64) {
	if s.taskDelivering == nil {
		s.taskDelivering = make(map[int64]bool)
	}
	s.taskDelivering[id] = true
}

// A task result that left the FIFO for good may be collected again if the
// database still lists it as undelivered.
func (s *Server) releaseTasksLocked(items []workflowDelivery) {
	for _, item := range items {
		delete(s.taskDelivering, item.TaskID)
	}
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

// The durable-task reconciler also retries transient result failures: an idle
// conversation starts its automatic turn, a running one shows what is waiting.
func (s *Server) retryWorkflowDeliveries() {
	s.chatMu.Lock()
	var batches []*workflowDeliveryBatch
	var running []*handoffTurn
	for session := range s.workflowPending {
		if batch := s.reserveWorkflowDeliveryLocked(session); batch != nil {
			batches = append(batches, batch)
		} else if turn := s.chatTurns[session]; turn != nil {
			running = append(running, turn)
		}
	}
	s.chatMu.Unlock()
	for _, batch := range batches {
		s.launchWorkflowDelivery(batch)
	}
	for _, turn := range running {
		turn.showWhenQuiet()
	}
}

func (s *Server) launchWorkflowDelivery(batch *workflowDeliveryBatch) {
	if batch != nil {
		go s.deliverWorkflowBatch(batch)
	}
}

// deliverWorkflowBatch is the idle path: nothing was running, so an automatic
// turn shows the waiting results and starts with them as its first message.
// Results that arrive once it runs are taken at its response boundaries.
func (s *Server) deliverWorkflowBatch(batch *workflowDeliveryBatch) {
	turn := batch.turn
	ctx := withHandoffTurn(batch.ctx, turn)
	defer func() {
		turn.end()
		batch.cancel(agent.AbortChatTurnFinished)
		s.finishConversationTurn(batch.session)
	}()
	turn.mu.Lock()
	c, err := turn.conversationLocked()
	var claimed []workflowDelivery
	if err == nil && c != nil && s.ctx.Err() == nil {
		turn.persistQueuedLocked()
		claimed = turn.claimLocked()
	}
	turn.mu.Unlock()
	if err != nil {
		log.Printf("[background] conversation %d lookup: %v", batch.id, err)
		s.chatMu.Lock()
		s.deferQueueHeadLocked(batch.session)
		s.chatMu.Unlock()
		return
	}
	if c == nil {
		s.dropSessionDeliveries(batch.session)
		return
	}
	if len(claimed) == 0 || batch.ctx.Err() != nil {
		return // Claimed results stay leased: ending the turn requeues them.
	}
	var prompts []string
	stopped := false
	for _, message := range claimed {
		stopped = stopped || message.Stopped
		if message.Prompt != "" {
			prompts = append(prompts, message.Prompt)
		}
	}
	if len(prompts) == 0 {
		turn.consume()
		if stopped {
			if err := s.finishRetestAfterBackgroundStop(ctx, c); err != nil {
				log.Printf("[retest] background stop: %v", err)
				s.chatMu.Lock()
				s.requeueDeliveriesLocked(batch.session, claimed, true)
				s.chatMu.Unlock()
			}
		}
		return
	}
	s.runConversationTurn(ctx, batch.cancel, c, backgroundReturnMessage(prompts), batch.session)
}

// deferQueueHeadLocked backs the FIFO head off after a failure that is not tied
// to one delivery, so the one-second reconciler does not retry it hot.
func (s *Server) deferQueueHeadLocked(session string) {
	if queue := s.workflowPending[session]; len(queue) > 0 {
		queue[0].Attempts = min(queue[0].Attempts+1, 6)
		queue[0].RetryAt = time.Now().Add(min(time.Second<<queue[0].Attempts, time.Minute))
	}
}

// A conversation that no longer exists receives nothing; deleting it never
// resurrects queued work.
func (s *Server) dropSessionDeliveries(session string) {
	s.chatMu.Lock()
	s.releaseTasksLocked(s.workflowPending[session])
	delete(s.workflowPending, session)
	s.chatMu.Unlock()
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
