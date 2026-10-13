package pibridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// ---- fakes and helpers ----

type fakeProvider struct {
	mu           sync.Mutex
	streamReqs   []llm.CompletionRequest
	completeReqs []llm.CompletionRequest
	stream       func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
	complete     func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (f *fakeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	f.mu.Lock()
	f.streamReqs = append(f.streamReqs, req)
	f.mu.Unlock()
	if f.stream == nil {
		panic("unexpected Stream call")
	}
	return f.stream(ctx, req)
}

func (f *fakeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	f.mu.Lock()
	f.completeReqs = append(f.completeReqs, req)
	f.mu.Unlock()
	if f.complete == nil {
		panic("unexpected Complete call")
	}
	return f.complete(ctx, req)
}

func (f *fakeProvider) calls() (stream, complete int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streamReqs), len(f.completeReqs)
}

func (f *fakeProvider) lastStreamReq(t *testing.T) llm.CompletionRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streamReqs) == 0 {
		t.Fatal("provider.Stream was never called")
	}
	return f.streamReqs[len(f.streamReqs)-1]
}

// seq builds a stream function that yields the given events, then ends.
func seq(evs ...llm.StreamEvent) func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
}

func textEvents(parts ...string) []llm.StreamEvent {
	evs := []llm.StreamEvent{{Type: llm.SEMessageStart}}
	for _, p := range parts {
		evs = append(evs, llm.StreamEvent{Type: llm.SETextDelta, Text: p})
	}
	return append(evs,
		llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		llm.StreamEvent{Type: llm.SEMessageStop})
}

func newTestRelay(t *testing.T, models ...Model) *Relay {
	t.Helper()
	return newTestRelayOpts(t, options{maxBody: maxBodyBytes, keepAlive: time.Hour}, models...)
}

func newTestRelayOpts(t *testing.T, opt options, models ...Model) *Relay {
	t.Helper()
	rl, err := newRelay(models, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })
	return rl
}

func (rl *Relay) try(ctx context.Context, method, path, token, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rl.BaseURL()+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

func (rl *Relay) do(t *testing.T, ctx context.Context, method, path, token, body string) *http.Response {
	t.Helper()
	resp, err := rl.try(ctx, method, path, token, body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// chat posts an authorized chat completion and returns status + full body.
func (rl *Relay) chat(t *testing.T, body string) (int, string, http.Header) {
	t.Helper()
	resp := rl.do(t, context.Background(), http.MethodPost, "/chat/completions", rl.APIKey(), body)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b), resp.Header
}

func simpleBody(model string, stream bool, extra string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]%s}`, model, stream, extra)
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Role             string  `json:"role"`
			Content          *string `json:"content"`
			ReasoningContent string  `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type sseResult struct {
	chunks   []wireChunk
	comments int
	done     bool
	raw      string
}

func parseSSE(t *testing.T, body string) sseResult {
	t.Helper()
	res := sseResult{raw: body}
	for _, block := range strings.Split(body, "\n\n") {
		switch {
		case block == "":
		case strings.HasPrefix(block, ":"):
			res.comments++
		case block == "data: [DONE]":
			res.done = true
		case strings.HasPrefix(block, "data: "):
			var c wireChunk
			if err := json.Unmarshal([]byte(block[len("data: "):]), &c); err != nil {
				t.Fatalf("bad SSE data frame %q: %v", block, err)
			}
			res.chunks = append(res.chunks, c)
		default:
			t.Fatalf("unexpected SSE block %q", block)
		}
	}
	return res
}

func (s sseResult) text() (content, reasoning string) {
	for _, c := range s.chunks {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				content += *ch.Delta.Content
			}
			reasoning += ch.Delta.ReasoningContent
		}
	}
	return
}

func (s sseResult) finish() string {
	for _, c := range s.chunks {
		for _, ch := range c.Choices {
			if ch.FinishReason != nil {
				return *ch.FinishReason
			}
		}
	}
	return ""
}

func (s sseResult) errorFrame() *struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
} {
	for _, c := range s.chunks {
		if c.Error != nil {
			return c.Error
		}
	}
	return nil
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ---- tests ----

func TestAuthAndRouting(t *testing.T) {
	fp := &fakeProvider{}
	rl := newTestRelay(t, Model{ID: "profile-1", Name: "Main", ContextWindow: 200000, MaxTokens: 8192, Provider: fp})

	host, _, err := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(rl.BaseURL(), "http://"), "/v1"))
	if err != nil || host != "127.0.0.1" || !strings.HasSuffix(rl.BaseURL(), "/v1") {
		t.Fatalf("BaseURL %q must be http://127.0.0.1:<port>/v1", rl.BaseURL())
	}
	other := newTestRelay(t)
	if rl.APIKey() == "" || rl.APIKey() == other.APIKey() {
		t.Fatal("APIKey must be non-empty and unique per relay")
	}
	if strings.Contains(fmt.Sprintf("%v %+v %s", rl, rl, rl), rl.APIKey()) {
		t.Fatal("formatting a Relay must not reveal the API key")
	}

	body := simpleBody("profile-1", true, "")
	for name, token := range map[string]string{"missing": "", "wrong": "nope", "prefix": rl.APIKey()[:10], "extended": rl.APIKey() + "x"} {
		resp := rl.do(t, context.Background(), http.MethodPost, "/chat/completions", token, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s token: status %d, want 401", name, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, rl.BaseURL()+"/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+rl.APIKey())
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("non-Bearer scheme must be 401, got %v %v", resp, err)
	}
	if s, c := fp.calls(); s+c != 0 {
		t.Fatalf("provider reached without valid auth (%d,%d)", s, c)
	}

	resp := rl.do(t, context.Background(), http.MethodGet, "/models", rl.APIKey(), "")
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextWindow int    `json:"contextWindow"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || resp.StatusCode != 200 ||
		len(list.Data) != 1 || list.Data[0].ID != "profile-1" || list.Data[0].ContextWindow != 200000 {
		t.Fatalf("GET /models = %d %+v (%v)", resp.StatusCode, list, err)
	}

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/chat/completions", 405},
		{http.MethodPost, "/models", 405},
		{http.MethodPost, "/embeddings", 404},
	} {
		r := rl.do(t, context.Background(), c.method, c.path, rl.APIKey(), "{}")
		r.Body.Close()
		if r.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, r.StatusCode, c.want)
		}
	}
}

func TestStreamTextReasoningAndUsage(t *testing.T) {
	fp := &fakeProvider{stream: seq(
		llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 40, CacheWriteTokens: 10}},
		llm.StreamEvent{Type: llm.SEThinkingDelta, Text: "think "},
		llm.StreamEvent{Type: llm.SEThinkingDelta, Text: "more"},
		llm.StreamEvent{Type: llm.SEThinkingSignature, Text: "sig"},
		llm.StreamEvent{Type: llm.SETextDelta, Text: "Hel"},
		llm.StreamEvent{Type: llm.SETextDelta, Text: "lo"},
		llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 7}},
		llm.StreamEvent{Type: llm.SEMessageStop},
	)}
	rl := newTestRelay(t, Model{ID: "global", MaxTokens: 4096, Provider: fp})

	body := `{"model":"global","stream":true,"stream_options":{"include_usage":true},
	  "messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],
	  "max_completion_tokens":99999,"temperature":0.5,"stop":"END","reasoning_effort":"none","store":false}`
	status, raw, hdr := rl.chat(t, body)
	if status != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q: %s", status, hdr.Get("Content-Type"), raw)
	}
	res := parseSSE(t, raw)
	if first := res.chunks[0].Choices[0].Delta; first.Role != "assistant" {
		t.Errorf("first chunk must carry the assistant role, got %+v", first)
	}
	if content, reasoning := res.text(); content != "Hello" || reasoning != "think more" {
		t.Errorf("content %q reasoning %q", content, reasoning)
	}
	if res.finish() != "stop" || !res.done || !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Errorf("finish %q done %t", res.finish(), res.done)
	}
	u := res.chunks[len(res.chunks)-1].Usage
	if u == nil || len(res.chunks[len(res.chunks)-1].Choices) != 0 {
		t.Fatalf("last chunk must be a choices-less usage chunk: %s", raw)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 7 || u.TotalTokens != 107 ||
		u.PromptTokensDetails.CachedTokens != 40 || u.PromptTokensDetails.CacheWriteTokens != 10 {
		t.Errorf("usage not preserved: %+v", u)
	}

	got := fp.lastStreamReq(t)
	if got.MaxTokens != 4096 || got.Thinking != "disabled" || !reflect.DeepEqual(got.Stop, []string{"END"}) ||
		got.Temperature == nil || *got.Temperature != 0.5 ||
		!reflect.DeepEqual(got.System, []string{"sys"}) || got.DynamicBoundary != 1 {
		t.Errorf("provider saw unexpected request: %+v", got)
	}

	// Without stream_options.include_usage no usage chunk is sent.
	_, raw, _ = rl.chat(t, simpleBody("global", true, ""))
	for _, c := range parseSSE(t, raw).chunks {
		if c.Usage != nil {
			t.Errorf("usage chunk sent without include_usage: %s", raw)
		}
	}
}

func TestStreamToolCallsFragmented(t *testing.T) {
	fp := &fakeProvider{stream: seq(
		llm.StreamEvent{Type: llm.SEMessageStart},
		llm.StreamEvent{Type: llm.SETextDelta, Text: "calling"},
		llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "toolu_1", ToolName: "read"},
		llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"pa`},
		llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `th":"a.go"`},
		llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `}`},
		llm.StreamEvent{Type: llm.SEToolUseStart, ToolName: "bash"}, // upstream sent no id
		llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"cmd":"ls"}`},
		// The Responses adapter reports end_turn even for function-call turns.
		llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"},
	)}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})

	_, raw, _ := rl.chat(t, simpleBody("m", true, ""))
	res := parseSSE(t, raw)

	type call struct{ id, name, args string }
	calls := map[int]*call{}
	for _, c := range res.chunks {
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				cur := calls[tc.Index]
				if cur == nil {
					if tc.ID == "" || tc.Type != "function" || tc.Function.Name == "" || tc.Function.Arguments != "" {
						t.Fatalf("tool call %d must start with id/type/name and empty arguments: %+v", tc.Index, tc)
					}
					cur = &call{id: tc.ID, name: tc.Function.Name}
					calls[tc.Index] = cur
					continue
				}
				if tc.ID != "" || tc.Function.Name != "" {
					t.Errorf("only the first chunk of tool call %d may carry id/name: %+v", tc.Index, tc)
				}
				cur.args += tc.Function.Arguments
			}
		}
	}
	if len(calls) != 2 || calls[0].id != "toolu_1" || calls[0].name != "read" || calls[0].args != `{"path":"a.go"}` {
		t.Errorf("call 0 wrong: %+v", calls[0])
	}
	if calls[1] == nil || !strings.HasPrefix(calls[1].id, "call_") || calls[1].name != "bash" || calls[1].args != `{"cmd":"ls"}` {
		t.Errorf("call 1 wrong (id must be minted when upstream sent none): %+v", calls[1])
	}
	if res.finish() != "tool_calls" || !res.done {
		t.Errorf("finish %q done %t", res.finish(), res.done)
	}
}

func TestHistoryWithToolResultsReachesProvider(t *testing.T) {
	fp := &fakeProvider{stream: seq(textEvents("ok")...)}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})

	body := `{"model":"m","stream":true,"tool_choice":"auto","messages":[
	  {"role":"system","content":"S1"},
	  {"role":"developer","content":[{"type":"text","text":"D1"}]},
	  {"role":"user","content":[{"type":"text","text":"hi"},{"type":"text","text":"there"}]},
	  {"role":"assistant","content":null,"reasoning_content":"plan","tool_calls":[
	    {"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.go\"}"}},
	    {"id":"call_b","type":"function","function":{"name":"ls","arguments":""}}]},
	  {"role":"tool","tool_call_id":"call_a","content":"file body"},
	  {"role":"tool","tool_call_id":"call_b","content":[{"type":"text","text":"x"},{"type":"text","text":"y"}]},
	  {"role":"user","content":"thanks"},
	  {"role":"assistant","content":"you're welcome"},
	  {"role":"assistant","content":null},
	  {"role":"user","content":"again"}],
	  "tools":[
	    {"type":"function","function":{"name":"read","description":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},
	    {"type":"function","function":{"name":"ls"}}]}`
	if status, raw, _ := rl.chat(t, body); status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}

	got := fp.lastStreamReq(t)
	wantMsgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi"), llm.TextBlock("there")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "plan"},
			{Type: llm.BlockToolUse, ID: "call_a", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)},
			{Type: llm.BlockToolUse, ID: "call_b", Name: "ls", Input: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolUseID: "call_a", Content: []llm.ContentBlock{llm.TextBlock("file body")}},
			{Type: llm.BlockToolResult, ToolUseID: "call_b", Content: []llm.ContentBlock{llm.TextBlock("x\ny")}},
		}},
		llm.UserText("thanks"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("you're welcome")}},
		llm.UserText("again"),
	}
	if !reflect.DeepEqual(got.Messages, wantMsgs) {
		gb, _ := json.MarshalIndent(got.Messages, "", " ")
		wb, _ := json.MarshalIndent(wantMsgs, "", " ")
		t.Errorf("messages mismatch\n got: %s\nwant: %s", gb, wb)
	}
	if !reflect.DeepEqual(got.System, []string{"S1", "D1"}) || got.DynamicBoundary != 2 {
		t.Errorf("system %v boundary %d", got.System, got.DynamicBoundary)
	}
	wantTools := []llm.ToolSchema{
		{Name: "read", Description: "Read", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}},
		{Name: "ls", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	}
	if !reflect.DeepEqual(got.Tools, wantTools) {
		t.Errorf("tools mismatch: %+v", got.Tools)
	}

	// tool_choice "none" validates but withholds the tools.
	body = strings.Replace(body, `"tool_choice":"auto"`, `"tool_choice":"none"`, 1)
	if status, raw, _ := rl.chat(t, body); status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	if got := fp.lastStreamReq(t); len(got.Tools) != 0 {
		t.Errorf("tool_choice none must send no tools, got %d", len(got.Tools))
	}
}

func TestInvalidRequestsAreRejectedBeforeTheProvider(t *testing.T) {
	fp := &fakeProvider{}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name, body string
		status     int
		contains   string
	}{
		{"invalid json", `{"model":`, 400, "invalid request body"},
		{"missing model", `{` + msgs + `}`, 400, "model is required"},
		{"unknown model", `{"model":"ghost",` + msgs + `}`, 404, "model_not_found"},
		{"no messages", `{"model":"m","messages":[]}`, 400, "non-empty"},
		{"only system", `{"model":"m","messages":[{"role":"system","content":"x"}]}`, 400, "at least one"},
		{"image input", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, 400, "image_url"},
		{"bad role", `{"model":"m","messages":[{"role":"function","content":"x"}]}`, 400, "unsupported role"},
		{"empty user", `{"model":"m","messages":[{"role":"user","content":""}]}`, 400, "no content"},
		{"tool without id", `{"model":"m","messages":[{"role":"tool","content":"x"}]}`, 400, "tool_call_id"},
		{"tool call args not object", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`, 400, "JSON object"},
		{"tool call args invalid", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{oops"}}]}]}`, 400, "JSON object"},
		{"tool call no id", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, 400, "id"},
		{"custom tool call", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"custom","custom":{}}]}]}`, 400, "unsupported tool call type"},
		{"non-function tool", `{"model":"m",` + msgs + `,"tools":[{"type":"web_search"}]}`, 400, "only function tools"},
		{"bad tool name", `{"model":"m",` + msgs + `,"tools":[{"type":"function","function":{"name":"bad name!"}}]}`, 400, "invalid"},
		{"duplicate tool", `{"model":"m",` + msgs + `,"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"a"}}]}`, 400, "duplicate"},
		{"tool schema not object", `{"model":"m",` + msgs + `,"tools":[{"type":"function","function":{"name":"a","parameters":"str"}}]}`, 400, "invalid request body"},
		{"forced tool_choice", `{"model":"m",` + msgs + `,"tool_choice":"required"}`, 400, "tool_choice"},
		{"n>1", `{"model":"m",` + msgs + `,"n":2}`, 400, "n must be 1"},
		{"negative max_tokens", `{"model":"m",` + msgs + `,"max_tokens":-1}`, 400, "max_tokens"},
		{"temperature", `{"model":"m",` + msgs + `,"temperature":5}`, 400, "temperature"},
		{"stop type", `{"model":"m",` + msgs + `,"stop":7}`, 400, "stop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := rl.chat(t, c.body)
			if status != c.status || !strings.Contains(body, c.contains) {
				t.Errorf("got %d %s; want %d containing %q", status, body, c.status, c.contains)
			}
			var env struct {
				Error struct{ Message, Type string }
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Message == "" || env.Error.Type == "" {
				t.Errorf("not an OpenAI error envelope: %s", body)
			}
		})
	}
	if s, c := fp.calls(); s+c != 0 {
		t.Fatalf("provider was called for invalid requests (%d,%d)", s, c)
	}

	small := newTestRelayOpts(t, options{maxBody: 256, keepAlive: time.Hour}, Model{ID: "m", Provider: fp})
	status, body, _ := small.chat(t, simpleBody("m", false, `,"pad":"`+strings.Repeat("x", 1024)+`"`))
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(body, "request_too_large") {
		t.Errorf("oversized body: %d %s", status, body)
	}
}

func TestNonStreamingModelAndNonStreamRequest(t *testing.T) {
	complete := func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockThinking, Thinking: "hmm"},
				llm.TextBlock("answer"),
				{Type: llm.BlockToolUse, Name: "read", Input: json.RawMessage(`{"path":"x"}`)}, // no id from upstream
			}}, "tool_use",
			llm.Usage{InputTokens: 50, OutputTokens: 5, CacheReadTokens: 20, CacheWriteTokens: 3}, nil
	}
	noStream := &fakeProvider{complete: complete}
	streaming := &fakeProvider{complete: complete}
	rl := newTestRelay(t,
		Model{ID: "nonstream", NonStreaming: true, Provider: noStream},
		Model{ID: "stream", Provider: streaming})

	// stream:true against a NonStreaming model → Complete adapted to SSE.
	_, raw, _ := rl.chat(t, simpleBody("nonstream", true, `,"stream_options":{"include_usage":true}`))
	res := parseSSE(t, raw)
	content, reasoning := res.text()
	if content != "answer" || reasoning != "hmm" || res.finish() != "tool_calls" || !res.done {
		t.Fatalf("adapted SSE wrong (content %q reasoning %q finish %q): %s", content, reasoning, res.finish(), raw)
	}
	var sawCall bool
	for _, c := range res.chunks {
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name == "read" {
					sawCall = strings.HasPrefix(tc.ID, "call_")
				}
			}
		}
	}
	if !sawCall {
		t.Errorf("tool call with minted id missing: %s", raw)
	}
	if u := res.chunks[len(res.chunks)-1].Usage; u == nil || u.PromptTokensDetails.CachedTokens != 20 || u.PromptTokensDetails.CacheWriteTokens != 3 {
		t.Errorf("usage cache fields lost: %+v", u)
	}
	if s, c := noStream.calls(); s != 0 || c != 1 {
		t.Errorf("NonStreaming model must use Complete only, got stream=%d complete=%d", s, c)
	}

	// stream:false against a normal model → Complete + chat.completion JSON.
	status, body, _ := rl.chat(t, simpleBody("stream", false, ""))
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || status != 200 || len(out.Choices) != 1 {
		t.Fatalf("bad JSON response %d: %s", status, body)
	}
	m := out.Choices[0].Message
	if out.Object != "chat.completion" || out.Model != "stream" || m.Content != "answer" || m.ReasoningContent != "hmm" ||
		len(m.ToolCalls) != 1 || !strings.HasPrefix(m.ToolCalls[0].ID, "call_") || m.ToolCalls[0].Function.Arguments != `{"path":"x"}` ||
		out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("unexpected completion: %s", body)
	}
	if out.Usage.PromptTokens != 50 || out.Usage.CompletionTokens != 5 || out.Usage.TotalTokens != 55 ||
		out.Usage.PromptTokensDetails.CachedTokens != 20 || out.Usage.PromptTokensDetails.CacheWriteTokens != 3 {
		t.Errorf("usage: %+v", out.Usage)
	}
	if s, c := streaming.calls(); s != 0 || c != 1 {
		t.Errorf("stream:false must use Complete only, got stream=%d complete=%d", s, c)
	}
}

func TestUpstreamErrorBeforeOutput(t *testing.T) {
	logs := captureLog(t)
	const secret = "sk-SECRETSECRET1234"
	errFn := func(code int) error {
		return fmt.Errorf(`openai: status %d: {"error":"bad key %s, Authorization: Bearer %s, got sk-proj-ab****wxyz"}`, code, secret, secret)
	}
	var code atomic.Int32
	fp := &fakeProvider{
		stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, errFn(int(code.Load()))) }
		},
		complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			return llm.Message{}, "", llm.Usage{}, errFn(int(code.Load()))
		},
	}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})

	for _, c := range []struct{ upstream, want int }{{429, 429}, {400, 400}, {401, 502}, {500, 502}, {503, 503}} {
		code.Store(int32(c.upstream))
		for _, stream := range []bool{true, false} {
			status, body, _ := rl.chat(t, simpleBody("m", stream, ""))
			if status != c.want {
				t.Errorf("upstream %d stream=%t → %d, want %d", c.upstream, stream, status, c.want)
			}
			if strings.Contains(body, secret) || strings.Contains(body, "sk-proj") || !strings.Contains(body, "upstream_status_") {
				t.Errorf("error body leaks the key or lacks the code: %s", body)
			}
		}
	}
	if out := logs.String(); !strings.Contains(out, "[pibridge]") || strings.Contains(out, secret) || strings.Contains(out, rl.APIKey()) {
		t.Errorf("failures must be logged without secrets:\n%s", out)
	}
}

func TestStreamFailuresAreErrorsNotNormalEnds(t *testing.T) {
	logs := captureLog(t)
	boom := errors.New("openai: connection reset by peer")
	cases := map[string]struct {
		stream func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
		code   string
	}{
		"error after output": {
			stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					if yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil) {
						yield(llm.StreamEvent{}, boom)
					}
				}
			},
			code: "upstream_failure",
		},
		"openai EOF without finish_reason": {
			stream: seq(
				llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"},
				llm.StreamEvent{Type: llm.SEMessageDelta}, // StopReason "" like openaiProvider on EOF
				llm.StreamEvent{Type: llm.SEMessageStop}),
			code: "upstream_truncated",
		},
		"anthropic EOF without message_delta": {
			stream: seq(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}),
			code:   "upstream_truncated",
		},
		"provider panic": {
			stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil)
					panic("kaboom")
				}
			},
			code: "upstream_failure",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rl := newTestRelay(t, Model{ID: "m", Provider: &fakeProvider{stream: c.stream}})
			status, raw, _ := rl.chat(t, simpleBody("m", true, ""))
			res := parseSSE(t, raw)
			if status != 200 {
				t.Fatalf("output had started, status must stay 200, got %d", status)
			}
			if content, _ := res.text(); content != "partial" {
				t.Errorf("content before failure lost: %q", content)
			}
			if e := res.errorFrame(); e == nil || e.Code != c.code || e.Message == "" {
				t.Errorf("want error frame %q, got %+v in %s", c.code, e, raw)
			}
			if res.finish() != "" || res.done {
				t.Errorf("a failed stream must not carry finish_reason or [DONE]: %s", raw)
			}
			// The relay keeps serving after a failed/panicking stream.
			resp := rl.do(t, context.Background(), http.MethodGet, "/models", rl.APIKey(), "")
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("relay unhealthy after failure: %d", resp.StatusCode)
			}
		})
	}
	if !strings.Contains(logs.String(), "stream failed") {
		t.Errorf("stream failures must be logged:\n%s", logs.String())
	}
}

func TestCancellationAndCloseReachTheProvider(t *testing.T) {
	started := make(chan struct{}, 4)
	cancelled := make(chan error, 4)
	fp := &fakeProvider{
		stream: func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "first"}, nil) {
					cancelled <- errors.New("consumer stopped")
					return
				}
				started <- struct{}{}
				<-ctx.Done()
				cancelled <- ctx.Err()
				yield(llm.StreamEvent{}, ctx.Err())
			}
		},
		complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			started <- struct{}{}
			<-ctx.Done()
			cancelled <- ctx.Err()
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		},
	}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})

	for _, stream := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			resp, err := rl.try(ctx, http.MethodPost, "/chat/completions", rl.APIKey(), simpleBody("m", stream, ""))
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			errc <- err
		}()
		waitFor(t, started, "provider start")
		cancel() // client disconnects mid-flight
		select {
		case err := <-cancelled:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("stream=%t: provider saw %v, want context.Canceled", stream, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stream=%t: client cancel never reached the provider", stream)
		}
		<-errc
	}

	// Close cancels in-flight upstream calls and returns promptly.
	go func() { // the connection is torn down by Close, so errors are expected here
		if resp, err := rl.try(context.Background(), http.MethodPost, "/chat/completions", rl.APIKey(), simpleBody("m", true, "")); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitFor(t, started, "provider start before Close")
	closed := make(chan struct{})
	go func() { rl.Close(); close(closed) }()
	waitFor(t, closed, "Close to return")
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Close: provider saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the in-flight upstream call")
	}
	if err := rl.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := net.DialTimeout("tcp", strings.TrimSuffix(strings.TrimPrefix(rl.BaseURL(), "http://"), "/v1"), time.Second); err == nil {
		t.Error("listener still accepting after Close")
	}
}

func TestKeepAliveCommitsHeadersWhileUpstreamIsSilent(t *testing.T) {
	release := make(chan struct{})
	fp := &fakeProvider{stream: func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			for _, ev := range textEvents("late") {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}}
	rl := newTestRelayOpts(t, options{maxBody: maxBodyBytes, keepAlive: 20 * time.Millisecond}, Model{ID: "m", Provider: fp})

	// Do returns once headers are committed — before the provider produced anything.
	resp := rl.do(t, context.Background(), http.MethodPost, "/chat/completions", rl.APIKey(), simpleBody("m", true, ""))
	defer resp.Body.Close()
	time.Sleep(80 * time.Millisecond)
	close(release)
	b, _ := io.ReadAll(resp.Body)
	res := parseSSE(t, string(b))
	if content, _ := res.text(); res.comments == 0 || content != "late" || !res.done {
		t.Errorf("keepalives %d content %q done %t: %s", res.comments, content, res.done, b)
	}
}

func TestConcurrentRequestsAndModelUpdates(t *testing.T) {
	const perModel = 6
	var arrived atomic.Int32
	allIn := make(chan struct{})
	label := func(name string, gate bool) *fakeProvider {
		return &fakeProvider{stream: func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				if gate { // hold every request until all of them are in flight together
					if arrived.Add(1) == 2*perModel {
						close(allIn)
					}
					select {
					case <-allIn:
					case <-ctx.Done():
						return
					}
				}
				for _, ev := range textEvents(name) {
					if !yield(ev, nil) {
						return
					}
				}
			}
		}}
	}
	rl := newTestRelay(t,
		Model{ID: "main", ContextWindow: 1000, Provider: label("A", true)},
		Model{ID: "sub", Provider: label("B", true)})

	var wg sync.WaitGroup
	errs := make(chan string, 2*perModel)
	for i := 0; i < perModel; i++ {
		for _, c := range []struct{ model, want string }{{"main", "A"}, {"sub", "B"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, raw, _ := rl.chat(t, simpleBody(c.model, true, ""))
				res := parseSSE(t, raw)
				if content, _ := res.text(); content != c.want || !res.done {
					errs <- fmt.Sprintf("%s got %q: %s", c.model, content, raw)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// Models() is a copy.
	snap := rl.Models()
	snap[0].ID = "tampered"
	if rl.Models()[0].ID != "main" {
		t.Fatal("Models() must return a copy")
	}

	// A request in flight keeps its provider across UpdateModels.
	hold := make(chan struct{})
	oldP := &fakeProvider{stream: func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			<-hold
			for _, ev := range textEvents("old") {
				yield(ev, nil)
			}
		}
	}}
	if err := rl.UpdateModels([]Model{{ID: "main", Provider: oldP}, {ID: "sub", Provider: label("B", false)}}); err != nil {
		t.Fatal(err)
	}
	inflight := make(chan string, 1)
	go func() {
		_, raw, _ := rl.chat(t, simpleBody("main", true, ""))
		c, _ := parseSSE(t, raw).text()
		inflight <- c
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if s, _ := oldP.calls(); s == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight request never reached the old provider")
		}
	}
	if err := rl.UpdateModels([]Model{{ID: "main", Provider: label("NEW", false)}}); err != nil {
		t.Fatal(err)
	}
	_, raw, _ := rl.chat(t, simpleBody("main", true, ""))
	if c, _ := parseSSE(t, raw).text(); c != "NEW" {
		t.Errorf("new request served by %q, want NEW", c)
	}
	if status, _, _ := rl.chat(t, simpleBody("sub", true, "")); status != 404 {
		t.Errorf("removed model must be 404, got %d", status)
	}
	close(hold)
	if c := <-inflight; c != "old" {
		t.Errorf("in-flight request switched providers: %q", c)
	}

	// Invalid updates are rejected atomically; the previous set stays live.
	for name, bad := range map[string][]Model{
		"duplicate":    {{ID: "x", Provider: oldP}, {ID: "x", Provider: oldP}},
		"nil provider": {{ID: "x"}},
		"bad id":       {{ID: "has space", Provider: oldP}},
		"negative":     {{ID: "x", MaxTokens: -1, Provider: oldP}},
	} {
		if err := rl.UpdateModels(bad); err == nil {
			t.Errorf("%s: UpdateModels accepted invalid models", name)
		}
	}
	if got := rl.Models(); len(got) != 1 || got[0].ID != "main" {
		t.Errorf("failed update changed the model set: %+v", got)
	}
	if _, err := NewRelay([]Model{{ID: "x"}}); err == nil {
		t.Error("NewRelay accepted a model without provider")
	}
}
