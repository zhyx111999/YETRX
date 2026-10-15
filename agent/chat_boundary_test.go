package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// boundaryLog is one ordered record of everything the contract talks about:
// provider traffic, activity emitted by the capture layer, and the three
// ChatControl callbacks. Tests assert the documented order against it.
type boundaryLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *boundaryLog) add(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *boundaryLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *boundaryLog) index(prefix string, from int) int {
	lines := l.snapshot()
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], prefix) {
			return i
		}
	}
	return -1
}

func (l *boundaryLog) count(prefix string) int {
	n := 0
	for _, line := range l.snapshot() {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func (l *boundaryLog) await(t *testing.T, prefix string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if i := l.index(prefix, 0); i >= 0 {
			return i
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never saw %q in %v", prefix, l.snapshot())
	return -1
}

// boundaryTurn scripts one provider response: text chunks (the stream waits on
// gate after the first one) followed by an optional tool call.
type boundaryTurn struct {
	chunks []string
	gate   chan struct{}
	tool   *llm.ContentBlock
}

func (turn boundaryTurn) message() llm.Message {
	content := []llm.ContentBlock{llm.TextBlock(strings.Join(turn.chunks, ""))}
	if turn.tool != nil {
		content = append(content, *turn.tool)
	}
	return llm.Message{Role: llm.RoleAssistant, Content: content}
}

func (turn boundaryTurn) stop() string {
	if turn.tool != nil {
		return "tool_use"
	}
	return "end_turn"
}

type boundaryProvider struct {
	log      *boundaryLog
	turns    []boundaryTurn
	reached  chan struct{} // closed once the first chunk of request 1 is out
	mu       sync.Mutex
	requests [][]llm.Message
}

func (p *boundaryProvider) next(req llm.CompletionRequest) boundaryTurn {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, append([]llm.Message(nil), req.Messages...))
	p.log.add("request:%d", len(p.requests))
	return p.turns[len(p.requests)-1]
}

func (p *boundaryProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	turn := p.next(req)
	return turn.message(), turn.stop(), llm.Usage{OutputTokens: 1}, nil
}

func (p *boundaryProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	turn := p.next(req)
	return func(yield func(llm.StreamEvent, error) bool) {
		for i, chunk := range turn.chunks {
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: chunk}, nil) {
				return
			}
			if i == 0 && turn.gate != nil {
				if p.reached != nil {
					close(p.reached)
				}
				select {
				case <-turn.gate:
				case <-ctx.Done():
					yield(llm.StreamEvent{}, ctx.Err())
					return
				}
			}
		}
		if turn.tool != nil {
			if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: turn.tool.ID, ToolName: turn.tool.Name}, nil) ||
				!yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(turn.tool.Input)}, nil) {
				return
			}
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: turn.stop(), Usage: llm.Usage{OutputTokens: 1}}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

// holdTool blocks until released: a tool that is still executing when the
// response that requested it has ended.
type holdTool struct {
	started  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	runs     int
	canceled bool
}

func installHoldTool(t *testing.T, name string) *holdTool {
	t.Helper()
	hold := &holdTool{started: make(chan struct{}, 4), release: make(chan struct{})}
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolResolve = nil
	tool := actool.Build(actool.Spec{Name: name, Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
		hold.mu.Lock()
		hold.runs++
		hold.mu.Unlock()
		hold.started <- struct{}{}
		select {
		case <-hold.release:
			return actool.Text("held tool finished"), nil
		case <-ctx.Done():
			hold.mu.Lock()
			hold.canceled = true
			hold.mu.Unlock()
			return actool.Errorf("cancelled"), nil
		}
	}})
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return []actool.CoreTool{tool}, DeferredInfo{}, func() {}
	}
	return hold
}

func holdCall(id string) *llm.ContentBlock {
	block := toolUse(id, "hold", `{}`)
	return &block
}

type boundaryHost struct {
	log   *boundaryLog
	mu    sync.Mutex
	queue []string
}

func (h *boundaryHost) offer(batch ...string) {
	h.mu.Lock()
	h.queue = append(h.queue, batch...)
	h.mu.Unlock()
}

func (h *boundaryHost) control() ChatControl {
	return ChatControl{
		DrainMessages: func() []string {
			h.mu.Lock()
			batch := h.queue
			h.queue = nil
			h.mu.Unlock()
			if len(batch) > 0 {
				h.log.add("drain:batch")
			} else {
				h.log.add("drain")
			}
			return batch
		},
		OnPrompt:      func() { h.log.add("prompt") },
		OnResponseEnd: func() { h.log.add("end") },
	}
}

func boundaryEmit(log *boundaryLog) func(db.Activity) {
	return func(a db.Activity) {
		if a.Kind == "usage" {
			return
		}
		log.add("emit:%s:%s", a.Kind, a.Detail)
	}
}

// The contract of ChatControl, end to end on the real ChatAgent, in both the
// streaming and the non-streaming mode.
func TestChatBoundaryContractOrdersCallbacksAroundAResponse(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "complete"}[stream], func(t *testing.T) {
			log, hold := &boundaryLog{}, installHoldTool(t, "hold")
			gate := make(chan struct{})
			provider := &boundaryProvider{log: log, reached: make(chan struct{}), turns: []boundaryTurn{
				{chunks: []string{"alpha ", "beta"}, gate: gate, tool: holdCall("hold-1")},
				{chunks: []string{"final answer"}},
			}}
			if !stream {
				provider.turns[0].gate = nil
			}
			root := t.TempDir()
			tx := transcript.NewStore(filepath.Join(root, "transcripts"))
			ca := NewChatAgent(provider, "fixture", root, tx, 100000)
			ca.SetNonStreaming(func() bool { return !stream })
			host := &boundaryHost{log: log}
			ctx := WithChatControl(context.Background(), host.control())
			type outcome struct {
				text string
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				text, err := ca.Chat(ctx, "agent", "parent", "original request", 0, 0, false, boundaryEmit(log))
				done <- outcome{text, err}
			}()

			if stream {
				select {
				case <-provider.reached:
				case <-time.After(10 * time.Second):
					t.Fatal("provider never started streaming")
				}
				time.Sleep(50 * time.Millisecond)
				if log.count("end") != 0 {
					t.Fatalf("response end reported while text was still streaming: %v", log.snapshot())
				}
				close(gate)
			}
			end := log.await(t, "end")
			select {
			case <-hold.started:
			case <-time.After(10 * time.Second):
				t.Fatal("tool never started")
			}
			lines := log.snapshot()
			if log.index("emit:text:alpha beta", 0) < 0 || log.index("emit:text:alpha beta", 0) > end || log.index("emit:tool_use:", 0) > end {
				t.Fatalf("response not fully recorded before OnResponseEnd: %v", lines)
			}
			if log.count("request:") != 1 || log.count("emit:tool_result:") != 0 {
				t.Fatalf("tool or next request ran before the boundary was observed: %v", lines)
			}
			host.offer("RESULT-1")
			close(hold.release)
			var result outcome
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("chat did not finish")
			}
			if result.err != nil || result.text != "final answer" {
				t.Fatalf("text=%q err=%v log=%v", result.text, result.err, log.snapshot())
			}

			// Documented order: end, tool result, then the drained batch is
			// accepted (OnPrompt) before the next request is sent.
			end = log.index("end", 0)
			toolResult := log.index("emit:tool_result:", end)
			batch := log.index("drain:batch", toolResult)
			prompt := log.index("prompt", batch)
			second := log.index("request:2", prompt)
			if end < 0 || toolResult < 0 || batch < 0 || prompt < 0 || second < 0 {
				t.Fatalf("callback order violated: %v", log.snapshot())
			}
			if log.count("prompt") != 2 || log.count("end") != 1 || log.count("request:") != 2 {
				t.Fatalf("a final response must not report a boundary: %v", log.snapshot())
			}
			hold.mu.Lock()
			defer hold.mu.Unlock()
			if hold.runs != 1 || hold.canceled {
				t.Fatalf("the executing tool was re-run or cancelled: runs=%d cancelled=%v", hold.runs, hold.canceled)
			}

			// The second request carries the batch as a user message AFTER the
			// completed tool result, never between tool_use and tool_result.
			provider.mu.Lock()
			payload := provider.requests[1]
			provider.mu.Unlock()
			last := len(payload) - 1
			if payload[last].Role != llm.RoleUser || payload[last].Text() != "RESULT-1" {
				t.Fatalf("batch is not the last user message: %+v", payload)
			}
			paired := payload[last-1]
			if paired.Role != llm.RoleUser || len(paired.Content) != 1 || paired.Content[0].Type != llm.BlockToolResult || paired.Content[0].ToolUseID != "hold-1" {
				t.Fatalf("tool result does not directly follow the tool call: %+v", payload)
			}
			records, err := tx.Load(tx.MainPath("parent"))
			if err != nil {
				t.Fatal(err)
			}
			messages, _ := transcript.Messages(records)
			delivered := 0
			for _, m := range messages {
				if m.Text() == "RESULT-1" {
					delivered++
				}
			}
			if delivered != 1 {
				t.Fatalf("batch recorded %d times", delivered)
			}
		})
	}
}

// A batch that was drained but never accepted (the run ended first) was not
// delivered: OnPrompt did not run for it, so the host can keep it.
func TestChatBoundaryUndeliveredBatchIsNeverAccepted(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "complete"}[stream], func(t *testing.T) {
			log := &boundaryLog{}
			hold := installHoldTool(t, "hold")
			close(hold.release)
			provider := &boundaryProvider{log: log, turns: []boundaryTurn{{chunks: []string{"working"}, tool: holdCall("hold-1")}, {chunks: []string{"never requested"}}}}
			ca := NewChatAgent(provider, "fixture", t.TempDir(), nil, 100000)
			ca.SetNonStreaming(func() bool { return !stream })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var drained int
			control := ChatControl{
				OnPrompt:      func() { log.add("prompt") },
				OnResponseEnd: func() { log.add("end") },
				DrainMessages: func() []string {
					drained++
					if drained == 2 { // after the tool round, before the second request
						cancel() // the foreground stop arrives exactly at the boundary
						return []string{"UNREAD-RESULT"}
					}
					return nil
				},
			}
			_, err := ca.Chat(WithChatControl(ctx, control), "agent", "stopped", "original request", 0, 0, false, boundaryEmit(log))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v log=%v", err, log.snapshot())
			}
			if log.count("prompt") != 1 || log.count("request:") != 1 {
				t.Fatalf("drained batch was accepted or sent after cancellation: %v", log.snapshot())
			}
		})
	}
}
