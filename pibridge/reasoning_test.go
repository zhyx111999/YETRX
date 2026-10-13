package pibridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

type wireDetail struct {
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	Signature string  `json:"signature"`
	Summary   *string `json:"summary"`
}

// piAppend is a port of appendOpenAIReasoningDetail in pi-ai
// (dist/api/openai-completions.js): adjacent reasoning.text entries merge into
// one, keeping only the first non-empty signature.
func piAppend(details []wireDetail, d wireDetail) []wireDetail {
	if n := len(details); n > 0 {
		last := &details[n-1]
		if d.Type == "reasoning.text" && last.Type == "reasoning.text" {
			last.Text += d.Text
			if last.Signature == "" {
				last.Signature = d.Signature
			}
			return details
		}
		if d.Type == "reasoning.summary" && last.Type == "reasoning.summary" {
			return details
		}
	}
	return append(details, d)
}

// streamedDetails returns the reasoning_details deltas of a stream, one slice
// per chunk that carried any.
func (s sseResult) streamedDetails() [][]wireDetail {
	var out [][]wireDetail
	for _, c := range s.chunks {
		for _, ch := range c.Choices {
			if len(ch.Delta.ReasoningDetails) > 0 {
				out = append(out, ch.Delta.ReasoningDetails)
			}
		}
	}
	return out
}

// piStored is what Pi keeps as the thinking block's replay signature.
func (s sseResult) piStored() []wireDetail {
	var stored []wireDetail
	for _, chunk := range s.streamedDetails() {
		for _, d := range chunk {
			stored = piAppend(stored, d)
		}
	}
	return stored
}

func TestReasoningAggregation(t *testing.T) {
	t.Run("signature fragments concatenate", func(t *testing.T) {
		var a reasoningAgg
		a.thinking("a")
		a.thinking("b")
		a.signature("s1")
		a.signature("s2")
		got := a.details()
		if len(got) != 1 || got[0].Type != "reasoning.text" || got[0].Text != "ab" || got[0].Signature != "s1s2" {
			t.Errorf("details: %+v", got)
		}
	})
	t.Run("signed blocks are separated so Pi cannot merge them", func(t *testing.T) {
		var a reasoningAgg
		a.thinking("one")
		a.signature("S1")
		a.thinking("two") // a thinking delta after a signature opens a new block
		a.signature("S2")
		got := a.details()
		if len(got) != 3 || got[0].Signature != "S1" || got[1].Type != "reasoning.summary" || got[1].Summary == nil || got[2].Text != "two" || got[2].Signature != "S2" {
			t.Fatalf("details: %+v", got)
		}
		var stored []wireDetail
		for _, d := range got {
			stored = piAppend(stored, wireDetail{Type: d.Type, Text: d.Text, Signature: d.Signature, Summary: d.Summary})
		}
		if len(stored) != 3 || stored[0].Signature != "S1" || stored[2].Signature != "S2" {
			t.Errorf("Pi's merge lost a signature: %+v", stored)
		}
	})
	t.Run("nothing to replay without a signed block", func(t *testing.T) {
		var a reasoningAgg
		a.thinking("unsigned")
		a.endBlock()
		a.signature("orphan") // no block in progress: dropped
		a.thinking("also unsigned")
		if got := a.details(); got != nil {
			t.Errorf("details: %+v", got)
		}
	})
	t.Run("text ends a block", func(t *testing.T) {
		var a reasoningAgg
		a.thinking("t")
		a.endBlock()
		a.signature("late")
		if a.details() != nil {
			t.Error("a signature after other content must not attach to the earlier block")
		}
	})
}

func TestStreamEmitsReasoningDetailsOnceBeforeFinish(t *testing.T) {
	fp := &fakeProvider{stream: seq(
		llm.StreamEvent{Type: llm.SEMessageStart},
		llm.StreamEvent{Type: llm.SEThinkingDelta, Text: "think "},
		llm.StreamEvent{Type: llm.SEThinkingDelta, Text: "more"},
		llm.StreamEvent{Type: llm.SEThinkingSignature, Text: "SIG-A|"},
		llm.StreamEvent{Type: llm.SEThinkingSignature, Text: "SIG-B"},
		llm.StreamEvent{Type: llm.SETextDelta, Text: "answer"},
		llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		llm.StreamEvent{Type: llm.SEMessageStop},
	)}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})
	_, raw, _ := rl.chat(t, simpleBody("m", true, ""))
	res := parseSSE(t, raw)

	if content, reasoning := res.text(); content != "answer" || reasoning != "think more" {
		t.Errorf("visible text wrong (reasoning must not be duplicated): content %q reasoning %q", content, reasoning)
	}
	chunks := res.streamedDetails()
	if len(chunks) != 1 || len(chunks[0]) != 1 {
		t.Fatalf("want exactly one reasoning_details chunk with one entry: %s", raw)
	}
	if d := chunks[0][0]; d.Type != "reasoning.text" || d.Text != "think more" || d.Signature != "SIG-A|SIG-B" {
		t.Errorf("detail: %+v", d)
	}
	detailsAt, finishAt := -1, -1
	for i, c := range res.chunks {
		for _, ch := range c.Choices {
			if len(ch.Delta.ReasoningDetails) > 0 {
				detailsAt = i
			}
			if ch.FinishReason != nil {
				finishAt = i
			}
		}
	}
	if detailsAt < 0 || detailsAt > finishAt || res.finish() != "stop" || !res.done {
		t.Errorf("reasoning_details (chunk %d) must precede the finish chunk (%d), finish %q done %t", detailsAt, finishAt, res.finish(), res.done)
	}

	// An upstream that never signs (OpenAI/Responses) gets no reasoning_details.
	unsigned := &fakeProvider{stream: seq(
		llm.StreamEvent{Type: llm.SEThinkingDelta, Text: "r"},
		llm.StreamEvent{Type: llm.SETextDelta, Text: "x"},
		llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})}
	rl2 := newTestRelay(t, Model{ID: "u", Provider: unsigned})
	_, raw, _ = rl2.chat(t, simpleBody("u", true, ""))
	if strings.Contains(raw, "reasoning_details") {
		t.Errorf("unsigned thinking must not produce reasoning_details: %s", raw)
	}
}

func TestCompleteKeepsThinkingSignature(t *testing.T) {
	complete := func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "hmm", Signature: "SIG-1"},
			{Type: llm.BlockThinking, Thinking: "then", Signature: "SIG-2"},
			llm.TextBlock("answer"),
		}}, "end_turn", llm.Usage{}, nil
	}
	rl := newTestRelay(t,
		Model{ID: "json", Provider: &fakeProvider{complete: complete}},
		Model{ID: "sse", NonStreaming: true, Provider: &fakeProvider{complete: complete}})

	// stream:false → chat.completion JSON.
	status, body, _ := rl.chat(t, simpleBody("json", false, ""))
	var out struct {
		Choices []struct {
			Message struct {
				ReasoningContent string       `json:"reasoning_content"`
				ReasoningDetails []wireDetail `json:"reasoning_details"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || status != 200 || len(out.Choices) != 1 {
		t.Fatalf("bad response %d: %s", status, body)
	}
	empty := ""
	want := []wireDetail{
		{Type: "reasoning.text", Text: "hmm", Signature: "SIG-1"},
		{Type: "reasoning.summary", Summary: &empty},
		{Type: "reasoning.text", Text: "then", Signature: "SIG-2"},
	}
	if m := out.Choices[0].Message; m.ReasoningContent != "hmmthen" || !reflect.DeepEqual(m.ReasoningDetails, want) {
		t.Errorf("non-streaming message lost a block or signature: %s", body)
	}

	// stream:true against a NonStreaming model → Complete adapted to SSE.
	_, raw, _ := rl.chat(t, simpleBody("sse", true, ""))
	res := parseSSE(t, raw)
	if st := res.piStored(); !reflect.DeepEqual(st, want) {
		t.Errorf("adapted stream lost a block or signature: %s", raw)
	}
}

func TestReasoningDetailsReplayRestoresSignedThinking(t *testing.T) {
	fp := &fakeProvider{stream: seq(textEvents("ok")...)}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})
	assistant := func(extra string) string {
		return `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"},
		  {"role":"assistant","content":null,` + extra + `,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		  {"role":"tool","tool_call_id":"c1","content":"out"}]}`
	}
	tool := llm.ContentBlock{Type: llm.BlockToolUse, ID: "c1", Name: "read", Input: json.RawMessage(`{}`)}
	assistantMsg := func(t *testing.T) []llm.ContentBlock {
		t.Helper()
		return fp.lastStreamReq(t).Messages[1].Content
	}
	cases := []struct {
		name, extra string
		want        []llm.ContentBlock
	}{
		{"signed details win over reasoning_content",
			`"reasoning_content":"plain","reasoning_details":[` +
				`{"type":"reasoning.text","text":"one","signature":"S1"},{"type":"reasoning.summary","summary":""},` +
				`{"type":"reasoning.text","text":"two","signature":"S2"}]`,
			[]llm.ContentBlock{
				{Type: llm.BlockThinking, Thinking: "one", Signature: "S1"},
				{Type: llm.BlockThinking, Thinking: "two", Signature: "S2"}, tool}},
		{"no details falls back to the plain field",
			`"reasoning_content":"plain"`,
			[]llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "plain"}, tool}},
		{"unsigned or foreign details are ignored",
			`"reasoning_content":"plain","reasoning_details":[{"type":"reasoning.encrypted","data":"x"},{"type":"reasoning.text","text":"t"}]`,
			[]llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "plain"}, tool}},
		{"malformed details do not fail the request",
			`"reasoning_details":"oops"`,
			[]llm.ContentBlock{tool}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if status, raw, _ := rl.chat(t, assistant(c.extra)); status != 200 {
				t.Fatalf("status %d: %s", status, raw)
			}
			if got := assistantMsg(t); !reflect.DeepEqual(got, c.want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(c.want)
				t.Errorf("assistant blocks\n got: %s\nwant: %s", gb, wb)
			}
		})
	}
}

// ---- real Anthropic adapter + fake upstream: two rounds of thinking + tool call ----

func anthFrame(typ, data string) string { return "event: " + typ + "\ndata: " + data + "\n\n" }

func anthThinking(s string) string {
	return anthFrame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":%q}}`, s))
}

func anthSignature(s string) string {
	return anthFrame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":%q}}`, s))
}

const anthToolCall = `{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}`

// anthTurn is a complete Anthropic stream: the given thinking/signature frames,
// then a read tool call.
func anthTurn(frames ...string) string {
	return anthFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`) +
		strings.Join(frames, "") +
		anthFrame("content_block_start", anthToolCall) +
		anthFrame("content_block_delta", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}`) +
		anthFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`) +
		anthFrame("message_stop", `{"type":"message_stop"}`)
}

// secondRound plays Pi: it stores the streamed reasoning_details the way pi-ai
// does (piAppend), then builds the next request exactly as pi-ai's
// convertMessages does for a thinking block with a stored signature — the
// assistant message carries reasoning_details and no reasoning_content — and
// sends it. It returns the Anthropic request body the upstream received.
func secondRound(t *testing.T, rl *Relay, up *upstream, res sseResult) string {
	t.Helper()
	assistant := map[string]any{
		"role": "assistant", "content": nil,
		"reasoning_details": res.piStored(),
		"tool_calls": []any{map[string]any{"id": "toolu_1", "type": "function",
			"function": map[string]any{"name": "read", "arguments": `{"path":"a.go"}`}}},
	}
	body, err := json.Marshal(map[string]any{"model": "global", "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		assistant,
		map[string]any{"role": "tool", "tool_call_id": "toolu_1", "content": "file body"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if status, raw, _ := rl.chat(t, string(body)); status != 200 {
		t.Fatalf("round 2 status %d: %s", status, raw)
	}
	_, sent := up.seen()
	return sent
}

func TestThinkingSignatureRoundTripThroughRealAnthropicAdapter(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   []map[string]any // thinking blocks the upstream must receive in round 2
	}{
		{"one block, signature in two fragments",
			[]string{anthThinking("First, "), anthThinking("read the file."), anthSignature("SIG-PART-1|"), anthSignature("SIG-PART-2")},
			[]map[string]any{{"type": "thinking", "thinking": "First, read the file.", "signature": "SIG-PART-1|SIG-PART-2"}}},
		{"two blocks keep their own signatures",
			[]string{anthThinking("plan A"), anthSignature("SIG-A"), anthThinking("plan B"), anthSignature("SIG-B")},
			[]map[string]any{
				{"type": "thinking", "thinking": "plan A", "signature": "SIG-A"},
				{"type": "thinking", "thinking": "plan B", "signature": "SIG-B"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newUpstream(t, anthTurn(c.frames...))
			p := realProvider(t, llm.Config{Format: llm.FormatAnthropic, BaseURL: up.URL, APIKey: realKey, Model: "claude-real", ThinkingType: "enabled"})
			rl := newTestRelay(t, Model{ID: "global", Provider: p})

			// Round 1: thinking + a tool call.
			_, raw, _ := rl.chat(t, `{"model":"global","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			res := parseSSE(t, raw)
			if _, reasoning := res.text(); reasoning == "" || res.finish() != "tool_calls" || !res.done {
				t.Fatalf("round 1: %s", raw)
			}
			// The thinking text is streamed once as reasoning_content; the signed
			// details are replay metadata only.
			var plain string
			for _, d := range res.piStored() {
				plain += d.Text
			}
			if _, reasoning := res.text(); plain != reasoning {
				t.Errorf("details text %q != streamed reasoning %q", plain, reasoning)
			}

			// Round 2: what the upstream receives must carry every signed block, in order.
			sent := secondRound(t, rl, up, res)
			var req struct {
				Thinking struct{ Type string } `json:"thinking"`
				Messages []struct {
					Role    string
					Content []map[string]any
				} `json:"messages"`
			}
			if err := json.Unmarshal([]byte(sent), &req); err != nil || len(req.Messages) != 3 {
				t.Fatalf("round 2 upstream body: %v %s", err, sent)
			}
			content := req.Messages[1].Content
			if req.Thinking.Type != "enabled" || len(content) != len(c.want)+1 {
				t.Fatalf("assistant content: %v (thinking %q)", content, req.Thinking.Type)
			}
			for i, w := range c.want {
				if !reflect.DeepEqual(content[i], w) {
					t.Errorf("thinking block %d\n got: %v\nwant: %v", i, content[i], w)
				}
			}
			if tu := content[len(content)-1]; tu["type"] != "tool_use" || tu["id"] != "toolu_1" {
				t.Errorf("tool_use block: %v", tu)
			}
		})
	}
}

func TestNonStreamingAnthropicCompleteKeepsSignature(t *testing.T) {
	// A real adapter, stream:false: the signature must survive parseAnthropicResponse.
	body := `{"type":"message","content":[` +
		`{"type":"thinking","thinking":"deep thought","signature":"SIG-NS"},` +
		`{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.go"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	p := realProvider(t, llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, APIKey: realKey, Model: "claude-real", ThinkingType: "enabled"})
	rl := newTestRelay(t, Model{ID: "global", Provider: p})

	status, out, _ := rl.chat(t, simpleBody("global", false, ""))
	var resp struct {
		Choices []struct {
			Message struct {
				ReasoningDetails []wireDetail `json:"reasoning_details"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || status != 200 || len(resp.Choices) != 1 ||
		len(resp.Choices[0].Message.ReasoningDetails) != 1 || resp.Choices[0].Message.ReasoningDetails[0].Signature != "SIG-NS" ||
		resp.Choices[0].Message.ReasoningDetails[0].Text != "deep thought" {
		t.Fatalf("non-streaming Anthropic signature lost (%d): %s", status, out)
	}
}
