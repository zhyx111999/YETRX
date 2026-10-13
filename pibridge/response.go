package pibridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// ---- wire types ----

type usageJSON struct {
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	TotalTokens         int           `json:"total_tokens"`
	PromptTokensDetails promptDetails `json:"prompt_tokens_details"`
}

// promptDetails carries the cache split. llm.Usage.InputTokens is the TOTAL
// input including cache, and the cache fields are subsets of it — exactly the
// OpenAI prompt_tokens / cached_tokens semantics. cache_write_tokens is the
// OpenRouter-style extension Pi reads.
type promptDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

func toUsageJSON(u llm.Usage) usageJSON {
	return usageJSON{
		PromptTokens:        u.InputTokens,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         u.InputTokens + u.OutputTokens,
		PromptTokensDetails: promptDetails{CachedTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens},
	}
}

type toolCallJSON struct {
	Index    *int          `json:"index,omitempty"` // streaming only
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function *functionJSON `json:"function,omitempty"`
}

type functionJSON struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type delta struct {
	Role             string            `json:"role,omitempty"`
	Content          *string           `json:"content,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ReasoningDetails []reasoningDetail `json:"reasoning_details,omitempty"`
	ToolCalls        []toolCallJSON    `json:"tool_calls,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usageJSON    `json:"usage,omitempty"`
}

type responseMessage struct {
	Role             string            `json:"role"`
	Content          *string           `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ReasoningDetails []reasoningDetail `json:"reasoning_details,omitempty"`
	ToolCalls        []toolCallJSON    `json:"tool_calls,omitempty"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type completionJSON struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usageJSON          `json:"usage"`
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

func newEnvelope(e *apiError) errorEnvelope {
	var env errorEnvelope
	env.Error.Message, env.Error.Type, env.Error.Code = e.msg, e.typ, e.code
	return env
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	if e.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, e.status, newEnvelope(e))
}

// ---- ids and finish reasons ----

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return hex.EncodeToString(b)
}

// toolID returns the upstream tool_use id, minting one when the upstream sent
// none. It is called exactly once per tool call, so the id is stable.
func toolID(id string) string {
	if id = strings.TrimSpace(id); id != "" {
		return id
	}
	return "call_" + randomHex(12)
}

// finishReason maps an llm stop reason to an OpenAI finish_reason.
// "tool_calls" iff tool calls were emitted: the Responses adapter reports
// end_turn even when the turn ended in function calls.
func finishReason(stop string, hasTools bool) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded", "length":
		return "length"
	case "refusal", "content_filter":
		return "content_filter"
	}
	if hasTools {
		return "tool_calls"
	}
	return "stop"
}

// ---- streaming ----

// sseWriter writes SSE frames. Headers are committed lazily so an upstream
// failure before any output can still be a proper HTTP error status.
type sseWriter struct {
	w         http.ResponseWriter
	rc        *http.ResponseController
	committed bool
	err       error // first write error (client gone)
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *sseWriter) write(frame string) {
	if s.err != nil {
		return
	}
	if !s.committed {
		s.committed = true
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		s.w.WriteHeader(http.StatusOK)
	}
	if _, err := io.WriteString(s.w, frame); err != nil {
		s.err = err
		return
	}
	if err := s.rc.Flush(); err != nil {
		s.err = err
	}
}

func (s *sseWriter) data(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.err = err
		return
	}
	s.write("data: " + string(b) + "\n\n")
}

// errTruncated marks a stream that ended without a finish reason.
var errTruncated = errors.New("upstream stream ended without a finish reason (truncated)")

// streamEncoder turns normalized llm.StreamEvents into OpenAI chat.completion.chunk
// frames: role, text / reasoning / tool_call deltas, finish_reason, usage, [DONE].
type streamEncoder struct {
	sse          *sseWriter
	id, model    string
	created      int64
	includeUsage bool
	roleSent     bool
	tools        int
	usage        llm.Usage
	stop         string // last non-empty stop reason; "" = stream never completed
	reasoning    reasoningAgg
}

func (e *streamEncoder) send(d delta, finish *string) {
	e.sse.data(chunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []chunkChoice{{Delta: d, FinishReason: finish}},
	})
}

func (e *streamEncoder) ensureRole() {
	if !e.roleSent {
		e.roleSent = true
		empty := ""
		e.send(delta{Role: "assistant", Content: &empty}, nil)
	}
}

func (e *streamEncoder) delta(d delta) {
	e.ensureRole()
	e.send(d, nil)
}

func (e *streamEncoder) event(ev llm.StreamEvent) {
	switch ev.Type {
	case llm.SEMessageStart:
		e.usage.Add(ev.Usage)
	case llm.SETextDelta:
		if ev.Text != "" {
			e.reasoning.endBlock()
			e.delta(delta{Content: &ev.Text})
		}
	case llm.SEThinkingDelta:
		if ev.Text != "" {
			e.reasoning.thinking(ev.Text)
			e.delta(delta{ReasoningContent: ev.Text})
		}
	case llm.SEThinkingSignature:
		e.reasoning.signature(ev.Text) // replay metadata: sent once, in finish
	case llm.SEToolUseStart:
		e.reasoning.endBlock()
		idx := e.tools
		e.tools++
		e.delta(delta{ToolCalls: []toolCallJSON{{
			Index: &idx, ID: toolID(ev.ToolID), Type: "function", Function: &functionJSON{Name: ev.ToolName},
		}}})
	case llm.SEToolInputJSON:
		if e.tools > 0 && ev.Text != "" {
			idx := e.tools - 1
			e.delta(delta{ToolCalls: []toolCallJSON{{Index: &idx, Function: &functionJSON{Arguments: ev.Text}}}})
		}
	case llm.SEMessageDelta:
		if ev.StopReason != "" {
			e.stop = ev.StopReason
		}
		e.usage.Add(ev.Usage)
	}
	// SEMessageStop has no Chat Completions equivalent.
}

// finish closes a completed stream. A stream with no stop reason was cut short
// (the OpenAI/Anthropic/Responses adapters all return silently on EOF), which is
// an error — never a normal end.
func (e *streamEncoder) finish() error {
	if e.stop == "" {
		return errTruncated
	}
	e.ensureRole() // an empty completion still gets its role chunk
	if details := e.reasoning.details(); len(details) > 0 {
		e.delta(delta{ReasoningDetails: details})
	}
	reason := finishReason(e.stop, e.tools > 0)
	e.send(delta{}, &reason)
	if e.includeUsage {
		u := toUsageJSON(e.usage)
		e.sse.data(chunk{
			ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
			Choices: []chunkChoice{}, Usage: &u,
		})
	}
	e.sse.write("data: [DONE]\n\n")
	return nil
}

// fail reports an error after the stream was committed. OpenAI clients (incl.
// the Node SDK Pi uses) raise on a data frame carrying an "error" object; no
// finish_reason and no [DONE] follow, so it cannot be mistaken for success.
func (e *streamEncoder) fail(apiErr *apiError) { e.sse.data(newEnvelope(apiErr)) }

// ---- non-streaming ----

// buildCompletion renders a full assistant message as a chat.completion body.
func buildCompletion(id, model string, created int64, msg llm.Message, stop string, usage llm.Usage) completionJSON {
	var text, reasoning strings.Builder
	var calls []toolCallJSON
	for _, b := range msg.Content {
		switch b.Type {
		case llm.BlockText:
			text.WriteString(b.Text)
		case llm.BlockThinking:
			reasoning.WriteString(b.Thinking)
		case llm.BlockToolUse:
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			calls = append(calls, toolCallJSON{ID: toolID(b.ID), Type: "function", Function: &functionJSON{Name: b.Name, Arguments: args}})
		}
	}
	m := responseMessage{Role: "assistant", ReasoningContent: reasoning.String(), ReasoningDetails: messageReasoningDetails(msg), ToolCalls: calls}
	if s := text.String(); s != "" || len(calls) == 0 {
		m.Content = &s // null only for a pure tool-call turn
	}
	return completionJSON{
		ID: id, Object: "chat.completion", Created: created, Model: model, Usage: toUsageJSON(usage),
		Choices: []completionChoice{{Message: m, FinishReason: finishReason(stop, len(calls) > 0)}},
	}
}

// completeEvents adapts Provider.Complete to the streaming event model so a
// NonStreaming model can serve stream:true clients through the same encoder.
func completeEvents(ctx context.Context, p llm.Provider, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if stop == "" {
			stop = "end_turn" // a complete non-streamed body is complete by definition
		}
		for _, b := range msg.Content {
			var evs []llm.StreamEvent
			switch b.Type {
			case llm.BlockThinking:
				evs = append(evs, llm.StreamEvent{Type: llm.SEThinkingDelta, Text: b.Thinking})
				if b.Signature != "" {
					evs = append(evs, llm.StreamEvent{Type: llm.SEThinkingSignature, Text: b.Signature})
				}
			case llm.BlockText:
				evs = append(evs, llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text})
			case llm.BlockToolUse:
				evs = append(evs, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name})
				if args := strings.TrimSpace(string(b.Input)); args != "" {
					evs = append(evs, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: args})
				}
			}
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

// ---- upstream errors ----

var (
	statusRe = regexp.MustCompile(`status (\d{3})`)
	// secretRe masks key-looking tokens (including OpenAI-style partially masked
	// ones like sk-abc***xyz) and api_key fields an upstream error body may echo.
	secretRe = regexp.MustCompile(`(?i)(bearer\s+|\bsk-|\bkey-)[A-Za-z0-9_\-.=*]{4,}|api[_-]?key["']?\s*[:=]\s*["']?[^\s"',}]{4,}`)
)

const maxErrorChars = 500

// scrub makes upstream error text safe to hand to Node and to log: relay key
// and key-shaped tokens masked, whitespace collapsed, length bounded.
func (rl *Relay) scrub(s string) string {
	s = strings.ReplaceAll(s, rl.apiKey, "[redacted]")
	s = secretRe.ReplaceAllString(s, "[redacted]")
	return truncateRunes(strings.Join(strings.Fields(s), " "), maxErrorChars)
}

// upstreamError converts a Provider error into an apiError. The upstream HTTP
// status (parsed from the llm package's "status NNN" error text) picks the relay
// status so Pi's own retry/overflow handling still sees 429 / 4xx / 5xx.
func (rl *Relay) upstreamError(err error) *apiError {
	e := &apiError{status: http.StatusBadGateway, typ: "upstream_error", code: "upstream_failure"}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.status, e.code = http.StatusGatewayTimeout, "upstream_timeout"
	case errors.Is(err, errTruncated):
		e.code = "upstream_truncated"
	default:
		if m := statusRe.FindStringSubmatch(err.Error()); m != nil {
			code, _ := strconv.Atoi(m[1])
			e.code = "upstream_status_" + m[1]
			switch code {
			case 400, 413, 422, 429, 503:
				e.status = code
			case 408, 504:
				e.status = http.StatusGatewayTimeout
			case 529:
				e.status = http.StatusServiceUnavailable
			}
		}
	}
	e.msg = "upstream request failed: " + rl.scrub(err.Error())
	return e
}
