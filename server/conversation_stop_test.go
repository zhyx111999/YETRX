package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
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

// The chat stop button follows the Pi plugin: it cancels only the current main
// turn. Subagents keep running, queued results are summarized once that turn has
// ended, and only deleting a conversation (or subagents_control stop) ends runs.

// conversationStopClient talks to the real mux, so routing, JWT middleware and
// the handler are all under test.
type conversationStopClient struct {
	t     *testing.T
	url   string
	token string
	http  *http.Client
}

func newConversationStopClient(t *testing.T, s *Server) *conversationStopClient {
	t.Helper()
	s.jwtKey = []byte("conversation-stop-test-key")
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &conversationStopClient{t: t, url: srv.URL, token: token, http: srv.Client()}
}

func (c *conversationStopClient) call(method string, id int64, suffix string) map[string]any {
	c.t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("%s/api/conversations/%d%s", c.url, id, suffix), strings.NewReader("{}"))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s %s: status %d, body %v, decode %v", method, suffix, resp.StatusCode, body, err)
	}
	return body
}

// stop returns the status reported by POST /api/conversations/{id}/stop.
func (c *conversationStopClient) stop(id int64) string {
	c.t.Helper()
	status, _ := c.call(http.MethodPost, id, "/stop")["status"].(string)
	return status
}

func (c *conversationStopClient) deleteConversation(id int64) {
	c.t.Helper()
	c.call(http.MethodDelete, id, "")
}

// stopGateSpec describes a model call that blocks until released or cancelled.
// A parent gate matches the latest user message; a subagent gate matches any
// message of its own transcript. The result must not contain any marker, because
// the result is later quoted back to the parent.
type stopGateSpec struct {
	marker, result string
	parent         bool
}

type stopGate struct {
	stopGateSpec
	started, release         chan struct{}
	startedOnce, releaseOnce sync.Once
}

// stopSemanticsProvider blocks the foreground turn and the subagents so a test
// can observe which of them an HTTP stop touches. Any other request is the
// automatic summary turn and is recorded.
type stopSemanticsProvider struct {
	mu        sync.Mutex
	gates     []*stopGate
	summaries []string
	cancelled map[string]bool
}

func newStopSemanticsProvider(specs ...stopGateSpec) *stopSemanticsProvider {
	p := &stopSemanticsProvider{cancelled: map[string]bool{}}
	for _, spec := range specs {
		p.gates = append(p.gates, &stopGate{stopGateSpec: spec, started: make(chan struct{}), release: make(chan struct{})})
	}
	return p
}

func (p *stopSemanticsProvider) gate(t *testing.T, marker string) *stopGate {
	t.Helper()
	for _, gate := range p.gates {
		if gate.marker == marker {
			return gate
		}
	}
	t.Fatalf("unknown gate %q", marker)
	return nil
}

func (p *stopSemanticsProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	var latest, history string
	for _, message := range req.Messages {
		history += message.Text() + "\n"
		if message.Role == llm.RoleUser {
			latest = message.Text()
		}
	}
	for _, gate := range p.gates {
		text := history
		if gate.parent {
			text = latest
		}
		if !strings.Contains(text, gate.marker) {
			continue
		}
		gate.startedOnce.Do(func() { close(gate.started) })
		select {
		case <-gate.release:
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(gate.result)}}, "end_turn", llm.Usage{}, nil
		case <-ctx.Done():
			p.mu.Lock()
			p.cancelled[gate.marker] = true
			p.mu.Unlock()
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		}
	}
	p.mu.Lock()
	p.summaries = append(p.summaries, latest)
	p.mu.Unlock()
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("PARENT_SUMMARY")}}, "end_turn", llm.Usage{}, nil
}

func (p *stopSemanticsProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, fmt.Errorf("fixture expects non-streaming"))
	}
}

func (p *stopSemanticsProvider) wasCancelled(marker string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelled[marker]
}

func (p *stopSemanticsProvider) summarized() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.summaries...)
}

type conversationStopFixture struct {
	t      *testing.T
	s      *Server
	e      *nativeEnv
	p      *stopSemanticsProvider
	client *conversationStopClient
}

func newConversationStopFixture(t *testing.T, specs ...stopGateSpec) (*conversationStopFixture, *db.Conversation) {
	t.Helper()
	s, e, _, conv := newNativeWorkflowServer(t)
	provider := newStopSemanticsProvider(specs...)
	root := t.TempDir()
	chat := agent.NewChatAgent(provider, "fixture", root, transcript.NewStore(filepath.Join(root, "transcripts")), 100000)
	chat.SetNonStreaming(func() bool { return true })
	s.chatAgent = chat
	return &conversationStopFixture{t: t, s: s, e: e, p: provider, client: newConversationStopClient(t, s)}, conv
}

func (f *conversationStopFixture) newConversation(title string) *db.Conversation {
	f.t.Helper()
	conv, err := f.e.pg.CreateConversation("agent", title, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = f.e.pg.DeleteConversation(conv.ID) })
	return conv
}

func (f *conversationStopFixture) awaitStarted(marker string) {
	f.t.Helper()
	select {
	case <-f.p.gate(f.t, marker).started:
	case <-time.After(10 * time.Second):
		f.t.Fatalf("model call %q never started", marker)
	}
}

func (f *conversationStopFixture) release(marker string) {
	gate := f.p.gate(f.t, marker)
	gate.releaseOnce.Do(func() { close(gate.release) })
}

// beginTurn starts a foreground turn whose model call blocks on marker.
func (f *conversationStopFixture) beginTurn(conv *db.Conversation, marker string) {
	f.t.Helper()
	session := f.s.convBusyKey(conv.ID)
	f.s.chatMu.Lock()
	f.s.chatBusy[session] = true
	f.s.chatMu.Unlock()
	f.s.runConversation(conv, marker, session, false, "")
	f.awaitStarted(marker)
}

// startSubagent dispatches one background child through the production tool.
func (f *conversationStopFixture) startSubagent(conv *db.Conversation, marker string) string {
	f.t.Helper()
	ctx := agent.WithRunInfo(f.s.ctx, agent.RunInfo{SessionID: f.s.convBusyKey(conv.ID)})
	input, _ := json.Marshal(map[string]any{"tasks": []workflow.Task{{Label: "child", AgentKey: f.e.keys[1], Task: marker}}})
	result, err := f.s.startWorkflow(ctx, input)
	if err != nil || result.IsError {
		f.t.Fatalf("start subagent: %s %v", result.Flatten(), err)
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &started); err != nil || started.RunID == "" {
		f.t.Fatalf("bad start result %q: %v", result.Flatten(), err)
	}
	f.awaitStarted(marker)
	return started.RunID
}

func (f *conversationStopFixture) busy(conv *db.Conversation) bool {
	f.s.chatMu.Lock()
	defer f.s.chatMu.Unlock()
	return f.s.chatBusy[f.s.convBusyKey(conv.ID)]
}

func (f *conversationStopFixture) runStatus(conv *db.Conversation, runID string) workflow.Status {
	f.t.Helper()
	run, err := f.s.workflows.Status(f.s.convBusyKey(conv.ID), runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run.Status
}

func (f *conversationStopFixture) awaitSummaries(count int) []string {
	f.t.Helper()
	waitNativeWorkflow(f.t, func() bool { return len(f.p.summarized()) >= count })
	return f.p.summarized()
}

func TestConversationStopHTTPKeepsRunningSubagentAndSummarizesItLater(t *testing.T) {
	const parent, child = "STOP_PARENT_REQUEST_RUNNING", "STOP_CHILD_REQUEST_RUNNING"
	f, conv := newConversationStopFixture(t,
		stopGateSpec{marker: parent, result: "PARENT_RESULT_RUNNING", parent: true},
		stopGateSpec{marker: child, result: "CHILD_RESULT_RUNNING"})
	f.beginTurn(conv, parent)
	runID := f.startSubagent(conv, child)

	if status := f.client.stop(conv.ID); status != "stopping" {
		t.Fatalf("stop status = %q", status)
	}
	waitNativeWorkflow(t, func() bool { return !f.busy(conv) })
	if !f.p.wasCancelled(parent) {
		t.Fatal("foreground model call survived the stop")
	}
	if f.runStatus(conv, runID) != workflow.Running || f.p.wasCancelled(child) {
		t.Fatal("chat stop reached the background subagent")
	}

	// The main turn is idle now: a second click must not touch the background.
	if status := f.client.stop(conv.ID); status != "idle" {
		t.Fatalf("idle stop status = %q", status)
	}
	if f.runStatus(conv, runID) != workflow.Running || f.p.wasCancelled(child) {
		t.Fatal("idle stop reached the background subagent")
	}

	// The subagent finishes after the turn: its result still reaches the parent.
	f.release(child)
	summaries := f.awaitSummaries(1)
	if !strings.Contains(summaries[0], runID) || !strings.Contains(summaries[0], "CHILD_RESULT_RUNNING") {
		t.Fatalf("summary prompt lost the subagent result: %q", summaries[0])
	}
	waitNativeWorkflow(t, func() bool { return !f.busy(conv) })
}

func TestConversationStopHTTPDeliversQueuedResultAfterStoppedTurn(t *testing.T) {
	const parent, child = "STOP_PARENT_REQUEST_QUEUED", "STOP_CHILD_REQUEST_QUEUED"
	f, conv := newConversationStopFixture(t,
		stopGateSpec{marker: parent, result: "PARENT_RESULT_QUEUED", parent: true},
		stopGateSpec{marker: child, result: "CHILD_RESULT_QUEUED"})
	session := f.s.convBusyKey(conv.ID)
	f.beginTurn(conv, parent)
	runID := f.startSubagent(conv, child)
	f.release(child)
	waitNativeWorkflow(t, func() bool {
		f.s.chatMu.Lock()
		defer f.s.chatMu.Unlock()
		for _, message := range f.s.workflowPending[session] {
			if message.Prompt != "" {
				return true
			}
		}
		return false
	})
	if len(f.p.summarized()) != 0 {
		t.Fatal("result interrupted the busy foreground turn")
	}

	if status := f.client.stop(conv.ID); status != "stopping" {
		t.Fatalf("stop status = %q", status)
	}
	summaries := f.awaitSummaries(1)
	if !f.p.wasCancelled(parent) {
		t.Fatal("foreground model call survived the stop")
	}
	if !strings.Contains(summaries[0], runID) || !strings.Contains(summaries[0], "CHILD_RESULT_QUEUED") {
		t.Fatalf("queued result was not summarized after the stopped turn: %q", summaries[0])
	}
	waitNativeWorkflow(t, func() bool { return !f.busy(conv) })
}

func TestConversationStopHTTPLeavesOtherConversationsAlone(t *testing.T) {
	const parentA, childA = "STOP_PARENT_REQUEST_ALPHA", "STOP_CHILD_REQUEST_ALPHA"
	const parentB, childB = "STOP_PARENT_REQUEST_BRAVO", "STOP_CHILD_REQUEST_BRAVO"
	f, a := newConversationStopFixture(t,
		stopGateSpec{marker: parentA, result: "PARENT_RESULT_ALPHA", parent: true},
		stopGateSpec{marker: childA, result: "CHILD_RESULT_ALPHA"},
		stopGateSpec{marker: parentB, result: "PARENT_RESULT_BRAVO", parent: true},
		stopGateSpec{marker: childB, result: "CHILD_RESULT_BRAVO"})
	b := f.newConversation("independent stop")
	f.beginTurn(a, parentA)
	f.beginTurn(b, parentB)
	runA, runB := f.startSubagent(a, childA), f.startSubagent(b, childB)

	if status := f.client.stop(a.ID); status != "stopping" {
		t.Fatalf("stop status = %q", status)
	}
	waitNativeWorkflow(t, func() bool { return !f.busy(a) })
	if !f.busy(b) || f.p.wasCancelled(parentB) {
		t.Fatal("stopping one conversation cancelled another conversation's turn")
	}
	for conv, run := range map[*db.Conversation]string{a: runA, b: runB} {
		if f.runStatus(conv, run) != workflow.Running {
			t.Fatalf("conversation %d lost its subagent", conv.ID)
		}
	}
	if f.p.wasCancelled(childA) || f.p.wasCancelled(childB) {
		t.Fatal("a subagent was cancelled")
	}

	f.release(parentB)
	waitNativeWorkflow(t, func() bool { return !f.busy(b) })
	f.release(childA)
	f.release(childB)
	summaries := f.awaitSummaries(2)
	joined := strings.Join(summaries, "\n---\n")
	for _, own := range []string{runA, runB} {
		if strings.Count(joined, own) != 1 {
			t.Fatalf("run %s delivered %d times: %q", own, strings.Count(joined, own), joined)
		}
	}
	for _, summary := range summaries {
		if strings.Contains(summary, "CHILD_RESULT_ALPHA") == strings.Contains(summary, "CHILD_RESULT_BRAVO") {
			t.Fatalf("one summary mixed (or lost) the conversations' results: %q", summary)
		}
	}
	waitNativeWorkflow(t, func() bool { return !f.busy(a) && !f.busy(b) })
}

func TestConversationDeleteHTTPStopsSubagentsAndDoesNotRevive(t *testing.T) {
	const parent, child = "STOP_PARENT_REQUEST_DELETE", "STOP_CHILD_REQUEST_DELETE"
	f, conv := newConversationStopFixture(t,
		stopGateSpec{marker: parent, result: "PARENT_RESULT_DELETE", parent: true},
		stopGateSpec{marker: child, result: "CHILD_RESULT_DELETE"})
	session := f.s.convBusyKey(conv.ID)
	f.beginTurn(conv, parent)
	runID := f.startSubagent(conv, child)

	f.client.deleteConversation(conv.ID)
	waitNativeWorkflow(t, func() bool { return !f.busy(conv) })
	if !f.p.wasCancelled(parent) || !f.p.wasCancelled(child) {
		t.Fatal("deleting the conversation left a foreground or background model call running")
	}
	run, err := f.s.workflows.Status(session, runID)
	if err != nil || run.Status != workflow.Stopped {
		t.Fatalf("subagent run after delete: %+v, %v", run.Status, err)
	}
	if c, err := f.e.pg.GetConversation(conv.ID); err != nil || c != nil {
		t.Fatalf("conversation still exists: %+v, %v", c, err)
	}
	waitNativeWorkflow(t, func() bool {
		f.s.chatMu.Lock()
		defer f.s.chatMu.Unlock()
		return !f.s.chatBusy[session] && len(f.s.workflowPending[session]) == 0
	})
	if summaries := f.p.summarized(); len(summaries) != 0 {
		t.Fatalf("deleted conversation was revived: %q", summaries)
	}
}

// Needs no database: the chat stop cancels only the registered foreground turn,
// while the manual subagents_control stop and the deletion path keep ending runs.
func TestChatStopCancelsOnlyItsForegroundTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 8)
	manager, err := workflow.New(ctx, t.TempDir(), 0, func(ctx context.Context, job workflow.Job) (string, error) {
		started <- job.OwnerSession
		<-ctx.Done()
		return "", ctx.Err()
	}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); manager.Wait() })
	s := &Server{ctx: ctx, workflows: manager, chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{},
		workflowPending: map[string][]workflowDelivery{}, workflowMuted: map[string]bool{}}

	session := map[int64]string{1: s.convBusyKey(1), 2: s.convBusyKey(2), 3: s.convBusyKey(3)}
	turn, run := map[int64]context.Context{}, map[int64]string{}
	for _, id := range []int64{1, 2} { // conversation 3 stays idle with a background run
		turn[id], _ = s.conversationRunContext(id, session[id])
		s.chatBusy[session[id]] = true
		s.workflowPending[session[id]] = []workflowDelivery{{Prompt: fmt.Sprintf("QUEUED_%d", id)}}
	}
	for id := int64(1); id <= 3; id++ {
		runID, err := manager.Start(session[id], "agent", 0, []workflow.Task{{Label: "child", Task: "background work"}})
		if err != nil {
			t.Fatal(err)
		}
		run[id] = runID
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("background run never started")
		}
	}
	status := func(id int64) workflow.Status {
		t.Helper()
		got, err := manager.Status(session[id], run[id])
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	queue := func(id int64) (muted bool, prompt string) {
		s.chatMu.Lock()
		defer s.chatMu.Unlock()
		return s.workflowMuted[session[id]], s.workflowPending[session[id]][0].Prompt
	}

	if !s.cancelConversationTurn(1) {
		t.Fatal("registered turn was not cancelled")
	}
	if !errors.Is(context.Cause(turn[1]), agent.AbortChatStoppedByUser) || turn[2].Err() != nil {
		t.Fatalf("turn causes: %v / %v", context.Cause(turn[1]), turn[2].Err())
	}
	for id := int64(1); id <= 3; id++ {
		if status(id) != workflow.Running {
			t.Fatalf("chat stop ended the background run of conversation %d", id)
		}
	}
	if muted, prompt := queue(1); muted || prompt != "QUEUED_1" {
		t.Fatalf("chat stop muted the queued reply: muted=%t prompt=%q", muted, prompt)
	}
	if s.cancelConversationTurn(3) || status(3) != workflow.Running {
		t.Fatal("stopping an idle main turn changed the background")
	}

	// The explicit control keeps working per run and per conversation.
	owner := context.WithValue(ctx, workflowCallerKey{}, workflowCaller{Session: session[1], Agent: "agent"})
	result, err := s.controlWorkflow(owner, json.RawMessage(fmt.Sprintf(`{"action":"stop","run_id":%q}`, run[1])))
	if err != nil || result.IsError {
		t.Fatalf("subagents_control stop: %s %v", result.Flatten(), err)
	}
	if status(1) != workflow.Stopped || status(2) != workflow.Running || status(3) != workflow.Running {
		t.Fatalf("manual stop statuses: %s %s %s", status(1), status(2), status(3))
	}

	// Deletion still cancels the turn and the runs, and silences the queued reply.
	s.cancelConversation(2)
	if !errors.Is(context.Cause(turn[2]), agent.AbortChatStoppedByUser) || status(2) != workflow.Stopped {
		t.Fatal("deleting a conversation left its turn or subagent running")
	}
	if muted, prompt := queue(2); !muted || prompt != "" {
		t.Fatalf("deletion kept the automatic reply: muted=%t prompt=%q", muted, prompt)
	}
	if status(3) != workflow.Running {
		t.Fatal("deleting one conversation stopped another conversation's subagent")
	}
}
