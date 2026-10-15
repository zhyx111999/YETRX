package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// handoffTurn is the host side of agent.ChatControl for one parent turn: a human
// message, a retest or schedule start, or the automatic turn the idle path
// started. It decides when waiting background results become visible and when
// the model reads them, and remembers what this turn has read.
//
//   - While a model response is in flight (from the request that DrainMessages
//     lets through until OnResponseEnd) nothing is written: no card appears
//     inside text the model is still generating.
//   - OnResponseEnd, and any arrival while the turn is between responses (its
//     tools running, no text streaming), write the cards at once. The tools are
//     neither cancelled nor re-run.
//   - DrainMessages hands displayed results to the next provider request and
//     OnPrompt records that the model now has them. Results the turn never read
//     (cancelled, budget, or a final response with no further request) stay in
//     the FIFO for the idle path's automatic follow-up.
//   - Everything the turn read is acknowledged when it ends.
//
// All callbacks and the cards share mu, so cards are written one at a time, in
// FIFO order, and never concurrently with the start of a response.
type handoffTurn struct {
	s         *Server
	session   string
	convID    int64
	ctx       context.Context
	automatic bool // started by the idle path to deliver results, not by a person or schedule

	mu         sync.Mutex
	conv       *db.Conversation
	responding bool
	done       bool
	stream     *conversationStream
	showQueued atomic.Bool // a goroutine already waits to display what is queued

	// protected by s.chatMu
	leased   []workflowDelivery // handed to the model, OnPrompt has not confirmed it yet
	received []workflowDelivery // in the transcript; acknowledged at end
	started  bool
}

type handoffTurnKey struct{}

func withHandoffTurn(ctx context.Context, turn *handoffTurn) context.Context {
	return context.WithValue(ctx, handoffTurnKey{}, turn)
}

func handoffTurnFrom(ctx context.Context) *handoffTurn {
	turn, _ := ctx.Value(handoffTurnKey{}).(*handoffTurn)
	return turn
}

func (s *Server) beginTurn(session string, id int64, ctx context.Context, c *db.Conversation, automatic bool) *handoffTurn {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	return s.beginTurnLocked(session, id, ctx, c, automatic)
}

func (s *Server) beginTurnLocked(session string, id int64, ctx context.Context, c *db.Conversation, automatic bool) *handoffTurn {
	turn := &handoffTurn{s: s, session: session, convID: id, ctx: ctx, conv: c, automatic: automatic}
	if s.chatTurns == nil {
		s.chatTurns = make(map[string]*handoffTurn)
	}
	s.chatTurns[session] = turn
	return turn
}

// control wires the turn into ChatAgent. The callbacks are plain data in and
// out; see agent.ChatControl for their contract.
func (t *handoffTurn) control() agent.ChatControl {
	return agent.ChatControl{OnPrompt: t.onPrompt, DrainMessages: t.drain, OnResponseEnd: t.onResponseEnd}
}

func (t *handoffTurn) bindStream(stream *conversationStream) {
	t.mu.Lock()
	t.stream = stream
	t.mu.Unlock()
}

func (t *handoffTurn) hasStarted() bool {
	t.s.chatMu.Lock()
	defer t.s.chatMu.Unlock()
	return t.started
}

// receivedResults lists what this turn's model has read, so the retest guard
// does not wait on results the model is already working from.
func (t *handoffTurn) receivedResults() (runKeys []string, taskIDs []int64) {
	if t == nil {
		return nil, nil
	}
	t.s.chatMu.Lock()
	defer t.s.chatMu.Unlock()
	for _, m := range t.received {
		if m.Prompt == "" {
			continue
		}
		if m.RunKey != "" {
			runKeys = append(runKeys, m.RunKey)
		}
		if m.TaskID != 0 {
			taskIDs = append(taskIDs, m.TaskID)
		}
	}
	return runKeys, taskIDs
}

// showWhenQuiet displays waiting results if the turn is between responses. A
// burst of arrivals (every step of a child run is one) shares one waiting
// goroutine: it reads the whole queue once it gets its turn.
func (t *handoffTurn) showWhenQuiet() {
	if t != nil && t.showQueued.CompareAndSwap(false, true) {
		go t.showBetweenResponses()
	}
}

func (t *handoffTurn) showBetweenResponses() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.showQueued.Store(false)
	if !t.done && !t.responding && t.ctx.Err() == nil {
		t.persistQueuedLocked()
	}
}

// onResponseEnd: the response that asked for tools is recorded; its cards go in
// now, while the tools may still run.
func (t *handoffTurn) onResponseEnd() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.responding = false
	if !t.done && t.ctx.Err() == nil {
		t.persistQueuedLocked()
	}
}

// drain runs before every provider request. Displayed results go to the model
// as one message after the tool results; with none, the request starts and the
// turn is responding until OnResponseEnd or its end.
func (t *handoffTurn) drain() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done || t.ctx.Err() != nil {
		return nil
	}
	t.persistQueuedLocked()
	var prompts []string
	for _, m := range t.claimLocked() {
		if m.Prompt != "" {
			prompts = append(prompts, m.Prompt)
		}
	}
	if len(prompts) == 0 {
		t.consume() // audit-only entries are already displayed
		t.responding = true
		return nil
	}
	return []string{backgroundReturnMessage(prompts)}
}

// onPrompt: the session accepted a user message into the transcript. Whatever
// was handed over for it is now read by the model.
func (t *handoffTurn) onPrompt() {
	t.s.chatMu.Lock()
	defer t.s.chatMu.Unlock()
	t.started = true
	t.received = append(t.received, t.leased...)
	t.leased = nil
}

func (t *handoffTurn) claimLocked() []workflowDelivery {
	t.s.chatMu.Lock()
	defer t.s.chatMu.Unlock()
	items := t.s.claimDeliveriesLocked(t.session)
	t.leased = append(t.leased, items...)
	return items
}

// consume retires leased deliveries that carry nothing for the model.
func (t *handoffTurn) consume() {
	t.s.chatMu.Lock()
	defer t.s.chatMu.Unlock()
	t.s.releaseTasksLocked(t.leased)
	t.leased = nil
}

func (t *handoffTurn) conversationLocked() (*db.Conversation, error) {
	if t.conv == nil {
		c, err := t.s.m.pg.GetConversation(t.convID)
		if err != nil || c == nil {
			return nil, err
		}
		t.conv = c
	}
	return t.conv, nil
}

// persistQueuedLocked writes the cards of queued results, oldest first, until
// every one is displayed or a write fails. Cards are idempotent: a task result
// keeps its single ledger row across retries, and a failed write keeps its
// FIFO place with a bounded backoff. mu is held.
func (t *handoffTurn) persistQueuedLocked() {
	s := t.s
	wrote := false
	defer func() {
		if wrote {
			_ = s.m.pg.TouchConversation(t.convID)
		}
	}()
	for s.ctx.Err() == nil {
		s.chatMu.Lock()
		index, message := firstUndisplayed(s.workflowPending[t.session])
		s.chatMu.Unlock()
		if index < 0 || time.Now().Before(message.RetryAt) {
			return
		}
		dropped, err := t.storeLocked(message)
		if err != nil {
			t.failDisplay(err)
			return
		}
		s.chatMu.Lock()
		// Only holders of mu change what is displayed and claims never take an
		// undisplayed entry, so it is still the first undisplayed one.
		queue := s.workflowPending[t.session]
		if i, _ := firstUndisplayed(queue); i >= 0 {
			if dropped {
				s.releaseTasksLocked(queue[i : i+1])
				s.workflowPending[t.session] = append(queue[:i:i], queue[i+1:]...)
				if len(s.workflowPending[t.session]) == 0 {
					delete(s.workflowPending, t.session)
				}
			} else {
				queue[i].ActivityStored = true
				wrote = true
			}
		}
		s.chatMu.Unlock()
	}
}

func firstUndisplayed(queue []workflowDelivery) (int, workflowDelivery) {
	for i, m := range queue {
		if !m.ActivityStored {
			return i, m
		}
	}
	return -1, workflowDelivery{}
}

// storeLocked displays one result. dropped means it needs no card: its task
// link is gone (conversation or task deleted) or already delivered.
func (t *handoffTurn) storeLocked(m workflowDelivery) (dropped bool, err error) {
	c, err := t.conversationLocked()
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, sql.ErrNoRows
	}
	activity := m.Activity
	if activity.Worker == "" {
		activity.Worker = c.AgentKey
	}
	pg := t.s.m.pg
	write := func() (int64, error) {
		if m.TaskID != 0 {
			return pg.AppendConversationTaskResult(t.s.ctx, m.TaskID, t.convID, m.TaskStatus, activity)
		}
		return pg.AppendConvActivity(t.convID, activity)
	}
	if t.stream != nil {
		err = t.stream.commit(write)
	} else {
		_, err = write()
	}
	if err != nil && m.TaskID != 0 && (errors.Is(err, sql.ErrNoRows) || errors.Is(err, db.ErrConversationTaskDelivered)) {
		return true, nil
	}
	return false, err
}

// failDisplay keeps the failed result at the head of the FIFO with a backoff
// (storage down), or forgets the whole queue if the conversation is gone.
func (t *handoffTurn) failDisplay(err error) {
	s := t.s
	if c, lookup := s.m.pg.GetConversation(t.convID); lookup == nil && c == nil {
		s.dropSessionDeliveries(t.session)
		return
	}
	log.Printf("[background] conversation %d delivery: %v", t.convID, err)
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	queue := s.workflowPending[t.session]
	if i, _ := firstUndisplayed(queue); i >= 0 {
		queue[i].Attempts = min(queue[i].Attempts+1, 6)
		queue[i].RetryAt = time.Now().Add(min(time.Second<<queue[i].Attempts, time.Minute))
	}
}

// end releases the turn. Results leased but never confirmed by OnPrompt return
// to the head of the FIFO; those the model read are acknowledged.
func (t *handoffTurn) end() {
	t.mu.Lock() // waits for a card write in flight
	t.done = true
	t.mu.Unlock()
	s := t.s
	s.chatMu.Lock()
	if s.chatTurns[t.session] == t {
		delete(s.chatTurns, t.session)
	}
	leased, received := t.leased, t.received
	t.leased, t.received = nil, nil
	s.requeueDeliveriesLocked(t.session, leased, !errors.Is(context.Cause(t.ctx), agent.AbortChatStoppedByUser))
	s.chatMu.Unlock()
	if s.ctx.Err() == nil {
		t.acknowledge(received)
	}
	s.chatMu.Lock()
	s.releaseTasksLocked(received)
	s.chatMu.Unlock()
}

func (t *handoffTurn) acknowledge(items []workflowDelivery) {
	s := t.s
	var tasks []int64
	s.chatMu.Lock()
	if s.workflowReported == nil {
		s.workflowReported = make(map[string]bool)
	}
	for _, m := range items {
		if m.Prompt == "" {
			continue
		}
		if m.RunKey != "" {
			s.workflowReported[m.RunKey] = true
		}
		if m.TaskID != 0 {
			tasks = append(tasks, m.TaskID)
		}
	}
	s.chatMu.Unlock()
	for _, id := range tasks {
		if err := s.m.pg.CompleteConversationTaskDelivery(s.ctx, id, t.convID); err != nil {
			log.Printf("[task delivery] acknowledge %d: %v", id, err)
		}
	}
}
