package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// These tests drive the real ChatAgent through a controlled provider and a real
// PostgreSQL ledger. The provider can hold a response open mid-stream and the
// `hold` tool can keep a tool running, so each test pins the moment at which a
// background result arrives: while text streams, while tools run, or after the
// last response.

type modelStep struct {
	chunks  []string
	gate    chan struct{} // the response stays open here after its first chunk
	reached chan struct{} // closed when the response is open at the gate
	tools   []llm.ContentBlock
}

func (s modelStep) message() llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: append([]llm.ContentBlock{llm.TextBlock(strings.Join(s.chunks, ""))}, s.tools...)}
}

func (s modelStep) stop() string {
	if len(s.tools) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

type responseModel struct {
	t         *testing.T
	mu        sync.Mutex
	steps     []modelStep
	requests  [][]llm.Message
	onRequest func(n int, messages []llm.Message)
}

func (m *responseModel) next(req llm.CompletionRequest) (int, modelStep) {
	m.mu.Lock()
	m.requests = append(m.requests, append([]llm.Message(nil), req.Messages...))
	n := len(m.requests)
	hook := m.onRequest
	var step modelStep
	if n <= len(m.steps) {
		step = m.steps[n-1]
	} else {
		m.t.Errorf("unscripted provider request %d", n)
		step = modelStep{chunks: []string{"UNSCRIPTED"}}
	}
	m.mu.Unlock()
	if hook != nil {
		hook(n, req.Messages)
	}
	return n, step
}

func (m *responseModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *responseModel) request(n int) []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[n-1]
}

func openGate(step modelStep, ctx context.Context) error {
	if step.gate == nil {
		return nil
	}
	if step.reached != nil {
		close(step.reached)
	}
	select {
	case <-step.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *responseModel) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	_, step := m.next(req)
	if err := openGate(step, ctx); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	return step.message(), step.stop(), llm.Usage{OutputTokens: 1}, nil
}

func (m *responseModel) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	_, step := m.next(req)
	return func(yield func(llm.StreamEvent, error) bool) {
		for i, chunk := range step.chunks {
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: chunk}, nil) {
				return
			}
			if i == 0 {
				if err := openGate(step, ctx); err != nil {
					yield(llm.StreamEvent{}, err)
					return
				}
			}
		}
		for _, tool := range step.tools {
			if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: tool.ID, ToolName: tool.Name}, nil) ||
				!yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(tool.Input)}, nil) {
				return
			}
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: step.stop(), Usage: llm.Usage{OutputTokens: 1}}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func toolCall(id, name, input string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

// holdTool executes until released. Its counters prove the tool is neither
// cancelled nor run again when a background result arrives.
type holdTool struct {
	started  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	runs     int
	finished int
	canceled bool
}

func installHoldTool(t *testing.T) *holdTool {
	t.Helper()
	hold := &holdTool{started: make(chan struct{}, 8), release: make(chan struct{})}
	previous := agent.ToolAugment
	t.Cleanup(func() { agent.ToolAugment = previous })
	tool := actool.Build(actool.Spec{Name: "hold", Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			hold.mu.Lock()
			hold.runs++
			hold.mu.Unlock()
			hold.started <- struct{}{}
			select {
			case <-hold.release:
				hold.mu.Lock()
				hold.finished++
				hold.mu.Unlock()
				return actool.Text("HOLD_DONE"), nil
			case <-ctx.Done():
				hold.mu.Lock()
				hold.canceled = true
				hold.mu.Unlock()
				return actool.Errorf("cancelled"), nil
			}
		}})
	agent.ToolAugment = func(ctx context.Context, key string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		var extra []actool.CoreTool
		var def agent.DeferredInfo
		cleanup := func() {}
		if previous != nil {
			extra, def, cleanup = previous(ctx, key)
		}
		return append(extra, tool), def, cleanup
	}
	return hold
}

func (h *holdTool) state() (runs, finished int, canceled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs, h.finished, h.canceled
}

func (h *holdTool) awaitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(10 * time.Second):
		t.Fatal("hold tool never started")
	}
}

type boundaryFixture struct {
	t       *testing.T
	s       *Server
	pg      *db.DB
	conv    *db.Conversation
	model   *responseModel
	hold    *holdTool
	session string
}

func newBoundaryFixture(t *testing.T, stream bool, steps ...modelStep) *boundaryFixture {
	t.Helper()
	s, e, _, conv := newNativeWorkflowServer(t)
	model := &responseModel{t: t, steps: steps}
	root := t.TempDir()
	chat := agent.NewChatAgent(model, "fixture", root, transcript.NewStore(filepath.Join(root, "transcripts")), 100000)
	chat.SetNonStreaming(func() bool { return !stream })
	s.chatAgent = chat
	return &boundaryFixture{t: t, s: s, pg: e.pg, conv: conv, model: model, hold: installHoldTool(t), session: s.convBusyKey(conv.ID)}
}

func (f *boundaryFixture) startTurn(message string) {
	f.s.chatMu.Lock()
	f.s.chatBusy[f.session] = true
	f.s.chatMu.Unlock()
	f.s.runConversation(f.conv, message, f.session, false, "")
}

func (f *boundaryFixture) finishRun(id string) {
	f.s.workflowCompleted(workflow.Run{ID: id, OwnerSession: f.session, Status: workflow.Succeeded, Tasks: []workflow.Task{{Label: "child", Result: "RESULT_" + id}}})
}

func runKey(id string) string { return workflowResultKey(workflow.Run{ID: id}) }

func (f *boundaryFixture) rows() []db.Activity {
	f.t.Helper()
	rows, _, err := deliveryActivityList(f.pg, f.conv.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func isCard(row db.Activity, id string) bool {
	return row.Tool == "subagents" && strings.Contains(row.Detail, "子代理运行 "+id)
}

func indexOfRow(rows []db.Activity, match func(db.Activity) bool) int {
	for i, row := range rows {
		if match(row) {
			return i
		}
	}
	return -1
}

func (f *boundaryFixture) cards(id string) int {
	n := 0
	for _, row := range f.rows() {
		if isCard(row, id) {
			n++
		}
	}
	return n
}

// awaitResponseEnded blocks until the first response has been recorded and its
// tools are the only thing still running.
func (f *boundaryFixture) awaitResponseEnded() {
	f.t.Helper()
	waitNativeWorkflow(f.t, func() bool {
		f.s.chatMu.Lock()
		turn := f.s.chatTurns[f.session]
		f.s.chatMu.Unlock()
		if turn == nil || f.model.count() == 0 {
			return false
		}
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return !turn.responding
	})
}

func (f *boundaryFixture) queued() []workflowDelivery {
	f.s.chatMu.Lock()
	defer f.s.chatMu.Unlock()
	return append([]workflowDelivery(nil), f.s.workflowPending[f.session]...)
}

func (f *boundaryFixture) reported(id string) bool {
	f.s.chatMu.Lock()
	defer f.s.chatMu.Unlock()
	return f.s.workflowReported[runKey(id)]
}

func requestJSON(messages []llm.Message) string {
	raw, _ := json.Marshal(messages)
	return string(raw)
}

// lastUserAfterToolResult asserts the protocol shape of a drained batch: the
// final user message follows a user message made of tool results only.
func lastUserAfterToolResult(t *testing.T, messages []llm.Message, toolID string) string {
	t.Helper()
	last := len(messages) - 1
	if last < 1 || messages[last].Role != llm.RoleUser || messages[last].Content[0].Type != llm.BlockText {
		t.Fatalf("no result message at the end of the request: %s", requestJSON(messages))
	}
	paired := messages[last-1]
	if paired.Role != llm.RoleUser || paired.Content[0].Type != llm.BlockToolResult || paired.Content[0].ToolUseID != toolID {
		t.Fatalf("result message does not directly follow the tool result %s: %s", toolID, requestJSON(messages))
	}
	return messages[last].Text()
}

func TestResponseBoundaryShowsResultsAfterTheResponseAndTheNextRequestReadsThem(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			gate, reached := make(chan struct{}), make(chan struct{})
			f := newBoundaryFixture(t, stream,
				modelStep{chunks: []string{"Plan: ", "inspect"}, gate: gate, reached: reached, tools: []llm.ContentBlock{toolCall("hold-1", "hold", `{}`)}},
				modelStep{chunks: []string{"All reviewed"}})
			f.startTurn("ORIGINAL_REQUEST")
			awaitDeliveryBoundary(t, reached)

			// A response is in flight: the result is held back whatever the mode.
			f.finishRun("wf-A")
			time.Sleep(250 * time.Millisecond)
			if f.cards("wf-A") != 0 {
				t.Fatal("a result card was inserted while the response was being generated")
			}
			if stream {
				if state := f.s.conversationStreamState(f.conv.ID); state.draft == nil || !strings.Contains(state.draft.Detail, "Plan: ") {
					t.Fatalf("expected the live draft while streaming: %+v", state.draft)
				}
			}
			if queued := f.queued(); len(queued) != 1 || queued[0].ActivityStored {
				t.Fatalf("held result must stay queued and invisible: %+v", queued)
			}

			// The response completes: the card appears while its tool still runs.
			close(gate)
			waitNativeWorkflow(t, func() bool { return f.cards("wf-A") == 1 })
			f.hold.awaitStarted(t)
			if runs, finished, canceled := f.hold.state(); runs != 1 || finished != 0 || canceled {
				t.Fatalf("arrival disturbed the running tool: runs=%d finished=%d canceled=%v", runs, finished, canceled)
			}
			if f.model.count() != 1 {
				t.Fatal("the next request started before the tool finished")
			}
			rows := f.rows()
			text := indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "text" && r.Detail == "Plan: inspect" })
			use := indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "tool_use" })
			card := indexOfRow(rows, func(r db.Activity) bool { return isCard(r, "wf-A") })
			if text < 0 || use < text || card < use || indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "tool_result" }) >= 0 {
				t.Fatalf("ledger order after the response: %+v", rows)
			}
			state := f.s.conversationStreamState(f.conv.ID)
			if state.draft != nil || state.cursor < rows[card].ID {
				t.Fatalf("draft must be withdrawn and followers told: draft=%+v cursor=%d card=%d", state.draft, state.cursor, rows[card].ID)
			}

			// Between responses, with no streaming text, an arrival shows at once.
			f.finishRun("wf-B")
			waitNativeWorkflow(t, func() bool { return f.cards("wf-B") == 1 })
			if f.model.count() != 1 {
				t.Fatal("a result arriving during tool execution started a request")
			}

			close(f.hold.release)
			waitRetestIdle(t, f.s)
			if f.model.count() != 2 {
				t.Fatalf("requests=%d: the results must ride the next request of the same call, with no follow-up turn", f.model.count())
			}
			message := lastUserAfterToolResult(t, f.model.request(2), "hold-1")
			a, b := strings.Index(message, "wf-A"), strings.Index(message, "wf-B")
			if a < 0 || b < a || !strings.HasPrefix(message, "以下是后台工作的返回") {
				t.Fatalf("results must arrive once, in FIFO order: %q", message)
			}
			rows = f.rows()
			cardA := indexOfRow(rows, func(r db.Activity) bool { return isCard(r, "wf-A") })
			cardB := indexOfRow(rows, func(r db.Activity) bool { return isCard(r, "wf-B") })
			toolResult := indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "tool_result" })
			final := indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "result" && r.Detail == "All reviewed" })
			if !(cardA < cardB && cardB < toolResult && toolResult < final) {
				t.Fatalf("ledger order: %+v", rows)
			}
			if f.cards("wf-A") != 1 || f.cards("wf-B") != 1 || len(f.queued()) != 0 || !f.reported("wf-A") || !f.reported("wf-B") {
				t.Fatalf("cards or acknowledgements wrong: queued=%+v", f.queued())
			}
			if runs, finished, canceled := f.hold.state(); runs != 1 || finished != 1 || canceled {
				t.Fatalf("tool executed %d times (cancelled=%v)", runs, canceled)
			}
		})
	}
}

func TestResponseBoundaryFinalResponseKeepsAutomaticFollowUp(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			gate, reached := make(chan struct{}), make(chan struct{})
			f := newBoundaryFixture(t, stream,
				modelStep{chunks: []string{"Final ", "answer"}, gate: gate, reached: reached},
				modelStep{chunks: []string{"Follow-up reviewed"}})
			f.startTurn("ORIGINAL_REQUEST")
			awaitDeliveryBoundary(t, reached)
			f.finishRun("wf-late")
			time.Sleep(150 * time.Millisecond)
			if f.cards("wf-late") != 0 {
				t.Fatal("card inserted into the final response")
			}
			close(gate)
			waitNativeWorkflow(t, func() bool { return f.model.count() == 2 })
			waitRetestIdle(t, f.s)
			if got := lastTextOf(f.model.request(2)); !strings.Contains(got, "wf-late") {
				t.Fatalf("follow-up did not carry the result: %q", got)
			}
			rows := f.rows()
			answer := indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "result" && r.Detail == "Final answer" })
			card := indexOfRow(rows, func(r db.Activity) bool { return isCard(r, "wf-late") })
			followUp := indexOfRow(rows, func(r db.Activity) bool { return r.Kind == "result" && r.Detail == "Follow-up reviewed" })
			if !(answer >= 0 && answer < card && card < followUp) || f.cards("wf-late") != 1 || !f.reported("wf-late") || len(f.queued()) != 0 {
				t.Fatalf("final-response ordering or accounting: %+v", rows)
			}
		})
	}
}

func lastTextOf(messages []llm.Message) string { return messages[len(messages)-1].Text() }

func TestResponseBoundaryStopKeepsDisplayedResultAndFollowsUpWithoutRerunningTheTool(t *testing.T) {
	f := newBoundaryFixture(t, true,
		modelStep{chunks: []string{"Starting"}, tools: []llm.ContentBlock{toolCall("hold-1", "hold", `{}`)}},
		modelStep{chunks: []string{"Handled after stop"}})
	f.startTurn("ORIGINAL_REQUEST")
	f.hold.awaitStarted(t)
	f.awaitResponseEnded()
	f.finishRun("wf-A")
	waitNativeWorkflow(t, func() bool { return f.cards("wf-A") == 1 })
	if _, _, canceled := f.hold.state(); canceled {
		t.Fatal("a result arrival cancelled the executing tool")
	}
	if !f.s.cancelConversationTurn(f.conv.ID) {
		t.Fatal("no foreground turn to stop")
	}
	waitNativeWorkflow(t, func() bool { return f.model.count() == 2 })
	waitRetestIdle(t, f.s)
	// The result never reached the stopped turn's model; the follow-up has it,
	// and its card was written exactly once.
	if strings.Contains(requestJSON(f.model.request(1)), "wf-A") {
		t.Fatal("first request already contained the result")
	}
	if !strings.Contains(requestJSON(f.model.request(2)), "Starting") || !strings.Contains(lastTextOf(f.model.request(2)), "wf-A") || f.cards("wf-A") != 1 || !f.reported("wf-A") {
		t.Fatalf("displayed but unread result lost on stop: cards=%d", f.cards("wf-A"))
	}
	if runs, _, _ := f.hold.state(); runs != 1 {
		t.Fatalf("tool re-run: %d", runs)
	}
}

func TestResponseBoundaryDeletionMutesDisplayedResultsAndNeverRevives(t *testing.T) {
	f := newBoundaryFixture(t, true,
		modelStep{chunks: []string{"Starting"}, tools: []llm.ContentBlock{toolCall("hold-1", "hold", `{}`)}},
		modelStep{chunks: []string{"must not be requested"}})
	f.startTurn("ORIGINAL_REQUEST")
	f.hold.awaitStarted(t)
	f.awaitResponseEnded()
	f.finishRun("wf-A")
	waitNativeWorkflow(t, func() bool { return f.cards("wf-A") == 1 })
	f.s.cancelConversation(f.conv.ID)
	if err := f.pg.DeleteConversation(f.conv.ID); err != nil {
		t.Fatal(err)
	}
	waitRetestIdle(t, f.s)
	time.Sleep(200 * time.Millisecond)
	if f.model.count() != 1 || len(f.queued()) != 0 {
		t.Fatalf("deleted conversation was revived: requests=%d queued=%+v", f.model.count(), f.queued())
	}
}

// A storage failure while showing keeps the result at the head of the FIFO with
// a backoff; the running turn is not blocked and nothing behind it overtakes.
func TestResponseBoundaryStorageFailureKeepsFifoAndRecoversWithoutDuplicates(t *testing.T) {
	f := newBoundaryFixture(t, true,
		modelStep{chunks: []string{"Working"}, tools: []llm.ContentBlock{toolCall("hold-1", "hold", `{}`)}},
		modelStep{chunks: []string{"Wrapped up"}},
		modelStep{chunks: []string{"Follow-up handled"}})
	if _, err := f.pg.Exec(`CREATE FUNCTION test_fail_boundary_activity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.tool='boundary_fixture' THEN RAISE EXCEPTION 'simulated activity storage failure'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER test_fail_boundary_activity BEFORE INSERT ON conversation_activities
FOR EACH ROW EXECUTE FUNCTION test_fail_boundary_activity()`); err != nil {
		t.Fatal(err)
	}
	clear := func() {
		_, _ = f.pg.Exec(`DROP TRIGGER IF EXISTS test_fail_boundary_activity ON conversation_activities; DROP FUNCTION IF EXISTS test_fail_boundary_activity()`)
	}
	t.Cleanup(clear)
	f.startTurn("ORIGINAL_REQUEST")
	f.hold.awaitStarted(t)
	f.awaitResponseEnded()
	f.s.queueWorkflowDelivery(f.session, workflowDelivery{Activity: db.Activity{Kind: "text", Tool: "boundary_fixture", Detail: "FIRST_RESULT"}, Prompt: "FIRST_RESULT", RunKey: "first"})
	f.s.queueWorkflowDelivery(f.session, workflowDelivery{Activity: db.Activity{Kind: "text", Detail: "SECOND_RESULT"}, Prompt: "SECOND_RESULT", RunKey: "second"})
	waitNativeWorkflow(t, func() bool {
		queued := f.queued()
		return len(queued) == 2 && queued[0].Attempts == 1 && queued[0].RetryAt.After(time.Now())
	})
	close(f.hold.release)
	waitRetestIdle(t, f.s)
	queued := f.queued()
	if f.model.count() != 2 || len(queued) != 2 || queued[0].RunKey != "first" || queued[1].RunKey != "second" || queued[0].ActivityStored || queued[1].ActivityStored {
		t.Fatalf("failed head must hold its place, nothing may overtake: requests=%d queued=%+v", f.model.count(), queued)
	}
	if strings.Contains(requestJSON(f.model.request(2)), "FIRST_RESULT") {
		t.Fatal("an undisplayed result reached the model")
	}
	clear()
	retryDeliveryNow(f.s, f.session)
	waitNativeWorkflow(t, func() bool { return f.model.count() == 3 })
	waitRetestIdle(t, f.s)
	message := lastTextOf(f.model.request(3))
	if strings.Index(message, "FIRST_RESULT") < 0 || strings.Index(message, "SECOND_RESULT") < strings.Index(message, "FIRST_RESULT") {
		t.Fatalf("recovered order: %q", message)
	}
	rows := f.rows()
	first := indexOfRow(rows, func(r db.Activity) bool { return r.Detail == "FIRST_RESULT" })
	second := indexOfRow(rows, func(r db.Activity) bool { return r.Detail == "SECOND_RESULT" })
	if first < 0 || second < first || !f.s.workflowReportedKey("first") || !f.s.workflowReportedKey("second") {
		t.Fatalf("recovered ledger: %+v", rows)
	}
}

func (s *Server) workflowReportedKey(key string) bool {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	return s.workflowReported[key]
}

// A retest may not conclude on a result whose card is merely displayed. The
// model must have read it, and the durable receipt is acknowledged only when
// the turn that read it ends.
func TestResponseBoundaryRetestDistinguishesDisplayedFromRead(t *testing.T) {
	s, fid := newRetestServer(t)
	c := beginTestRetest(t, s, fid)
	task := linkedTestTask(t, s, c.ID)
	session := s.convBusyKey(c.ID)
	verdict := `{"verdict":"inconclusive","summary":"reviewed the delegated evidence","evidence":"Actual fixture evidence from the returned exploration result."}`
	var midTurn struct {
		mu               sync.Mutex
		pendingWhileRead bool
		rejected         bool
		sawResult        bool
	}
	model := &responseModel{t: t, steps: []modelStep{
		{chunks: []string{"Checking"}, tools: []llm.ContentBlock{toolCall("hold-1", "hold", `{}`), toolCall("record-1", "record_finding_retest_result", verdict)}},
		{tools: []llm.ContentBlock{toolCall("record-2", "record_finding_retest_result", verdict)}},
		{chunks: []string{"Verdict saved"}},
	}}
	model.onRequest = func(n int, messages []llm.Message) {
		if n != 2 {
			return
		}
		pending, err := s.m.pg.ConversationHasPendingTasks(context.Background(), c.ID, nil)
		midTurn.mu.Lock()
		defer midTurn.mu.Unlock()
		midTurn.pendingWhileRead = err == nil && pending // read by the model, not yet acknowledged
		body := requestJSON(messages)
		midTurn.rejected = strings.Contains(body, "尚未处理完")
		midTurn.sawResult = strings.Contains(body, fmt.Sprintf("task_id=%d", task.ID))
	}
	s.chatAgent = agent.NewChatAgent(model, "test", s.m.dir, nil, 10000)
	hold := installHoldTool(t)
	f := &boundaryFixture{t: t, s: s, pg: s.m.pg, conv: c, model: model, hold: hold, session: session}

	runTestRetest(s, c, "retest the finding")
	hold.awaitStarted(t)
	f.awaitResponseEnded()
	if err := s.m.pg.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.collectTaskDeliveries(); err != nil {
		t.Fatal(err)
	}
	waitNativeWorkflow(t, func() bool {
		for _, row := range f.rows() {
			if row.Tool == "spawn_task" {
				return true
			}
		}
		return false
	})
	// Displayed: the card exists and the link has an activity, but nothing has
	// been read, so the durable state still blocks a conclusion.
	var activity, delivered bool
	if err := s.m.pg.QueryRow(`SELECT activity_id IS NOT NULL, delivered_at IS NOT NULL FROM conversation_tasks WHERE task_id=$1`, task.ID).Scan(&activity, &delivered); err != nil || !activity || delivered {
		t.Fatalf("displayed receipt wrong: activity=%v delivered=%v err=%v", activity, delivered, err)
	}
	if pending, err := s.m.pg.ConversationHasPendingTasks(context.Background(), c.ID, nil); err != nil || !pending {
		t.Fatalf("a displayed result must still block the verdict: %v %v", pending, err)
	}
	if model.count() != 1 {
		t.Fatal("the model was asked before the tool finished")
	}

	close(hold.release) // record-1 now runs in the same response, before the model has read the result
	waitRetestIdle(t, s)
	midTurn.mu.Lock()
	defer midTurn.mu.Unlock()
	if !midTurn.rejected || !midTurn.sawResult || !midTurn.pendingWhileRead {
		t.Fatalf("premature verdict was not refused, or result unread: rejected=%v sawResult=%v pendingWhileRead=%v", midTurn.rejected, midTurn.sawResult, midTurn.pendingWhileRead)
	}
	if model.count() != 3 {
		t.Fatalf("requests=%d", model.count())
	}
	assertRetestStatus(t, s, c.ID, "completed")
	if pending, err := s.m.pg.ConversationHasPendingTasks(context.Background(), c.ID, nil); err != nil || pending {
		t.Fatalf("the read result was not acknowledged when its turn ended: %v %v", pending, err)
	}
	r, err := s.m.pg.FindingRetestForConversation(context.Background(), c.ID)
	if err != nil || r == nil || r.Verdict != "inconclusive" {
		t.Fatalf("verdict: %+v %v", r, err)
	}
}

// Results that exceed one batch are read in FIFO order as consecutive user
// messages before the next request; none is dropped, split or reordered, and
// audit-only entries are displayed without producing a message.
func TestResponseBoundaryBatchesLargeResultsWithinTheCap(t *testing.T) {
	f := newBoundaryFixture(t, true,
		modelStep{chunks: []string{"Working"}, tools: []llm.ContentBlock{toolCall("hold-1", "hold", `{}`)}},
		modelStep{chunks: []string{"Read everything"}})
	f.startTurn("ORIGINAL_REQUEST")
	f.hold.awaitStarted(t)
	f.awaitResponseEnded()
	f.s.queueWorkflowDelivery(f.session, workflowDelivery{Activity: db.Activity{Worker: "subagent:child", Kind: "text", Detail: "AUDIT_ONLY"}})
	for _, key := range []string{"one", "two", "three"} {
		f.s.queueWorkflowDelivery(f.session, workflowDelivery{
			Activity: db.Activity{Kind: "text", Tool: "subagents", Detail: "CARD_" + key},
			Prompt:   "RESULT_" + key + strings.Repeat("x", 40<<10), RunKey: key})
	}
	waitNativeWorkflow(t, func() bool {
		return indexOfRow(f.rows(), func(r db.Activity) bool { return r.Detail == "CARD_three" }) >= 0
	})
	if f.model.count() != 1 {
		t.Fatal("results started a request while the tool was running")
	}
	close(f.hold.release)
	waitRetestIdle(t, f.s)
	if f.model.count() != 2 {
		t.Fatalf("requests=%d", f.model.count())
	}
	messages := f.model.request(2)
	var batches []string
	for i := len(messages) - 1; i > 0 && messages[i].Role == llm.RoleUser && messages[i].Content[0].Type == llm.BlockText; i-- {
		batches = append([]string{messages[i].Text()}, batches...)
	}
	if len(batches) != 3 {
		t.Fatalf("expected three batches after the tool result, got %d: %s", len(batches), requestJSON(messages))
	}
	for i, key := range []string{"one", "two", "three"} {
		if !strings.Contains(batches[i], "RESULT_"+key) || len(batches[i]) > maxBatchPromptBytes+len(backgroundReturnHeader) {
			t.Fatalf("batch %d out of order or over the cap (%d bytes)", i, len(batches[i]))
		}
		if !f.s.workflowReportedKey(key) {
			t.Fatalf("%s not acknowledged", key)
		}
	}
	if strings.Contains(requestJSON(messages), "AUDIT_ONLY") || len(f.queued()) != 0 {
		t.Fatal("audit-only entry leaked to the model or the queue was not drained")
	}
}
