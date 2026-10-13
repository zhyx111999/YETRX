package pibridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// These tests put the REAL norma OpenAI and Anthropic adapters behind the relay,
// talking to a local fake upstream (no network, no real LLM). They pin the
// assumptions the relay makes about adapter behaviour: usage folding, stop
// reasons, EOF truncation, and that the real key only ever goes upstream.

type upstream struct {
	*httptest.Server
	mu      sync.Mutex
	headers http.Header
	body    string
}

func newUpstream(t *testing.T, sse string) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.headers, u.body = r.Header.Clone(), string(b)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) seen() (http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers, u.body
}

func realProvider(t *testing.T, cfg llm.Config) llm.Provider {
	t.Helper()
	cfg.MaxRetries, cfg.EmptyResponseRetries = -1, -1
	p, err := llm.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sseFrames(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(f + "\n\n")
	}
	return b.String()
}

const realKey = "sk-REAL-UPSTREAM-KEY-0001"

func TestOpenAIAdapterBehindRelay(t *testing.T) {
	up := newUpstream(t, sseFrames(
		`data: {"choices":[{"delta":{"reasoning_content":"r1"}}]}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":12}}}`,
		`data: [DONE]`))
	p := realProvider(t, llm.Config{Format: llm.FormatOpenAI, BaseURL: up.URL + "/v1", APIKey: realKey, Model: "gpt-real"})
	rl := newTestRelay(t, Model{ID: "profile-1", Provider: p})

	status, raw, _ := rl.chat(t, simpleBody("profile-1", true, `,"stream_options":{"include_usage":true},"max_tokens":77`))
	res := parseSSE(t, raw)
	content, reasoning := res.text()
	last := res.chunks[len(res.chunks)-1].Usage
	if status != 200 || content != "hi" || reasoning != "r1" || res.finish() != "tool_calls" || !res.done ||
		last == nil || last.PromptTokens != 30 || last.CompletionTokens != 4 || last.PromptTokensDetails.CachedTokens != 12 {
		t.Fatalf("unexpected relay output (%d): %s", status, raw)
	}
	var args string
	for _, c := range res.chunks {
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
		}
	}
	if args != `{"p":1}` {
		t.Errorf("tool arguments = %q", args)
	}

	hdr, body := up.seen()
	if hdr.Get("Authorization") != "Bearer "+realKey || !strings.Contains(body, `"model":"gpt-real"`) || !strings.Contains(body, `"max_tokens":77`) {
		t.Errorf("upstream saw header %q body %s", hdr.Get("Authorization"), body)
	}
	if strings.Contains(raw, realKey) || strings.Contains(raw, "gpt-real") {
		t.Errorf("real key / upstream model leaked to the client: %s", raw)
	}
}

func TestOpenAIAdapterTruncatedStreamIsAnError(t *testing.T) {
	// Upstream closes without finish_reason / [DONE]: the adapter ends quietly, the relay must not.
	up := newUpstream(t, sseFrames(`data: {"choices":[{"delta":{"content":"cut o"}}]}`))
	p := realProvider(t, llm.Config{Format: llm.FormatOpenAI, BaseURL: up.URL + "/v1", APIKey: realKey, Model: "gpt-real"})
	rl := newTestRelay(t, Model{ID: "m", Provider: p})

	_, raw, _ := rl.chat(t, simpleBody("m", true, ""))
	res := parseSSE(t, raw)
	if e := res.errorFrame(); e == nil || e.Code != "upstream_truncated" || res.finish() != "" || res.done {
		t.Fatalf("truncated stream must surface as an error frame: %s", raw)
	}
}

func TestAnthropicAdapterBehindRelay(t *testing.T) {
	up := newUpstream(t, strings.Join([]string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":40,"cache_creation_input_tokens":5,"output_tokens":1}}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"sig"}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_7","name":"read"}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\""}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":":1}"}}` + "\n\n",
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n",
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
	}, ""))
	p := realProvider(t, llm.Config{Format: llm.FormatAnthropic, BaseURL: up.URL, APIKey: realKey, Model: "claude-real"})
	rl := newTestRelay(t, Model{ID: "global", Provider: p})

	_, raw, _ := rl.chat(t, `{"model":"global","stream":true,"stream_options":{"include_usage":true},
	  "messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	res := parseSSE(t, raw)
	content, reasoning := res.text()
	u := res.chunks[len(res.chunks)-1].Usage
	if content != "ok" || reasoning != "hmm" || res.finish() != "tool_calls" || !res.done || u == nil {
		t.Fatalf("unexpected relay output: %s", raw)
	}
	// Anthropic input = 10 + 40 cache read + 5 cache write, with the cache split preserved.
	if u.PromptTokens != 55 || u.CompletionTokens != 9 || u.TotalTokens != 64 ||
		u.PromptTokensDetails.CachedTokens != 40 || u.PromptTokensDetails.CacheWriteTokens != 5 {
		t.Errorf("usage: %+v", u)
	}
	hdr, body := up.seen()
	var sent struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil || hdr.Get("X-Api-Key") != realKey ||
		len(sent.System) != 1 || sent.System[0].Text != "be brief" || len(sent.System[0].CacheControl) == 0 {
		t.Errorf("upstream saw key %q body %s (%v)", hdr.Get("X-Api-Key"), body, err)
	}
	if strings.Contains(raw, realKey) {
		t.Error(fmt.Sprintf("real key leaked to the client: %s", raw))
	}
}
