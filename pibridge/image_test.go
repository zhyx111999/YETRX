package pibridge

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

const (
	pngB64     = "iVBORw0KGgo="
	jpgB64     = "/9j/4AAQ"
	pngDataURL = "data:image/png;base64," + pngB64
	jpgDataURL = "data:image/jpeg;base64," + jpgB64
)

// piImageBody is the request Pi sends after a tool returned an image: the tool
// message carries the text, and a separate user message the image.
const piImageBody = `{"model":"m","stream":true,"messages":[
  {"role":"user","content":[{"type":"text","text":"what is in this"},{"type":"image_url","image_url":{"url":"` + pngDataURL + `"}},
    {"type":"image_url","image_url":{"url":"https://example.com/cat.webp"}}]},
  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"screenshot","arguments":"{}"}}]},
  {"role":"tool","tool_call_id":"c1","content":"(see attached image)"},
  {"role":"user","content":[{"type":"text","text":"Attached image(s) from tool result:"},{"type":"image_url","image_url":{"url":"` + jpgDataURL + `"}}]}]}`

func TestImageInputsReachTheProvider(t *testing.T) {
	fp := &fakeProvider{stream: seq(textEvents("ok")...)}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})
	if status, raw, _ := rl.chat(t, piImageBody); status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	want := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock("what is in this"),
			llm.ImageBase64Block("image/png", pngB64),
			llm.ImageURLBlock("https://example.com/cat.webp")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "c1", Name: "screenshot", Input: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("c1", "(see attached image)", false)}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock("Attached image(s) from tool result:"), llm.ImageBase64Block("image/jpeg", jpgB64)}},
	}
	if got := fp.lastStreamReq(t).Messages; !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("messages\n got: %s\nwant: %s", gb, wb)
	}
}

func TestImageInsideToolMessageStaysInTheToolResult(t *testing.T) {
	fp := &fakeProvider{stream: seq(textEvents("ok")...)}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"},
	    {"type":"image_url","image_url":{"url":"` + pngDataURL + `"}},{"type":"text","text":"c"}]}]}`
	if status, raw, _ := rl.chat(t, body); status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	want := llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: "c1", Content: []llm.ContentBlock{
		llm.TextBlock("a\nb"), llm.ImageBase64Block("image/png", pngB64), llm.TextBlock("c")}}
	if got := fp.lastStreamReq(t).Messages[2].Content; !reflect.DeepEqual(got, []llm.ContentBlock{want}) {
		gb, _ := json.Marshal(got)
		t.Errorf("tool_result must keep its image, in place: %s", gb)
	}
}

func TestInvalidImagesAreRejectedBeforeTheProvider(t *testing.T) {
	fp := &fakeProvider{}
	rl := newTestRelay(t, Model{ID: "m", Provider: fp})
	img := func(role, url string) string {
		return `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"` + role + `","content":[{"type":"image_url","image_url":{"url":"` + url + `"}}]}]}`
	}
	secret := "TOPSECRETPAYLOAD"
	cases := []struct{ name, body, contains string }{
		{"system image", `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"` + pngDataURL + `"}}]},{"role":"user","content":"x"}]}`, "only supported in user and tool"},
		{"ftp url", img("user", "ftp://example.com/a.png"), "http(s)"},
		{"file url", img("user", "file:///etc/passwd"), "http(s)"},
		{"javascript url", img("user", "javascript:alert(1)"), "http(s)"},
		{"no host", img("user", "https:///a.png"), "http(s)"},
		{"bare path", img("user", "a.png"), "http(s)"},
		{"not an image type", img("user", "data:text/plain;base64,"+secret), "unsupported image media type"},
		{"svg", img("user", "data:image/svg+xml;base64,"+secret), "unsupported image media type"},
		{"not base64", img("user", "data:image/png,"+secret), "must be base64"},
		{"bad base64", img("user", "data:image/png;base64,!!"+secret), "not valid base64"},
		{"line breaks in base64", img("user", "data:image/png;base64,iVBO\\nRw0="), "not valid base64"},
		{"empty data", img("user", "data:image/png;base64,"), "not valid base64"},
		{"missing image_url", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, "no image_url"},
		{"audio part", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}]}`, "unsupported content part type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := rl.chat(t, c.body)
			if status != 400 || !strings.Contains(body, c.contains) {
				t.Errorf("got %d %s; want 400 containing %q", status, body, c.contains)
			}
			if strings.Contains(body, secret) {
				t.Errorf("error echoes the image payload: %s", body)
			}
		})
	}
	if s, c := fp.calls(); s+c != 0 {
		t.Fatalf("provider was called for invalid images (%d,%d)", s, c)
	}
}

// ---- the three real encoders behind the relay ----

// A minimal complete upstream stream per wire format, so the relay finishes.
var (
	openAIDone = sseFrames(
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `data: [DONE]`)
	responsesDone = sseFrames(
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"ok"}`,
		`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"status":"completed"}}`)
	anthropicDone = anthFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`) +
		anthFrame("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`) +
		anthFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`) +
		anthFrame("message_stop", `{"type":"message_stop"}`)
)

func TestImagesReachEveryProviderFormat(t *testing.T) {
	// Pi's image request plus an image nested in a tool message.
	body := `{"model":"m","stream":true,"messages":[
	  {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + pngDataURL + `"}},
	    {"type":"image_url","image_url":{"url":"https://example.com/cat.webp"}}]},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"done"},{"type":"image_url","image_url":{"url":"` + jpgDataURL + `"}}]}]}`

	cases := []struct {
		name     string
		cfg      llm.Config
		sse      string
		contains []string
		absent   []string
	}{
		{"anthropic", llm.Config{Format: llm.FormatAnthropic}, anthropicDone,
			[]string{
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}`,
				`{"type":"image","source":{"type":"url","url":"https://example.com/cat.webp"}}`,
				// nested in the tool_result, not flattened into text
				`"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"done"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + jpgB64 + `"}}]`,
			}, nil},
		{"openai chat", llm.Config{Format: llm.FormatOpenAI}, openAIDone,
			[]string{
				`{"type":"image_url","image_url":{"url":"` + pngDataURL + `"}}`,
				`{"type":"image_url","image_url":{"url":"https://example.com/cat.webp"}}`,
				// the tool message keeps its text; the image rides in the user message after it
				`{"role":"tool","content":"done","tool_call_id":"c1"}`,
				`{"role":"user","content":[{"type":"text","text":"Attached image(s) from tool result:"},{"type":"image_url","image_url":{"url":"` + jpgDataURL + `"}}]}`,
			}, []string{`"content":"` + jpgB64}},
		{"openai responses", llm.Config{Format: llm.FormatOpenAIResponses}, responsesDone,
			[]string{
				`{"type":"input_image","image_url":"` + pngDataURL + `","detail":"auto"}`,
				`{"type":"input_image","image_url":"https://example.com/cat.webp","detail":"auto"}`,
				`{"type":"function_call_output","call_id":"c1","output":"done"}`,
				`{"role":"user","content":[{"type":"input_text","text":"Attached image(s) from tool result:"},{"type":"input_image","image_url":"` + jpgDataURL + `","detail":"auto"}]}`,
			}, []string{`"output":"data:`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newUpstream(t, c.sse)
			cfg := c.cfg
			cfg.BaseURL, cfg.APIKey, cfg.Model = up.URL+"/v1", realKey, "real-model"
			if cfg.Format == llm.FormatAnthropic {
				cfg.BaseURL = up.URL
			}
			rl := newTestRelay(t, Model{ID: "m", Provider: realProvider(t, cfg)})
			status, raw, _ := rl.chat(t, body)
			if status != 200 || !strings.Contains(raw, `"content":"ok"`) {
				t.Fatalf("relay answered %d: %s", status, raw)
			}
			_, sent := up.seen()
			for _, want := range c.contains {
				if !strings.Contains(sent, want) {
					t.Errorf("upstream body lacks %s\nbody: %s", want, sent)
				}
			}
			for _, bad := range c.absent {
				if strings.Contains(sent, bad) {
					t.Errorf("upstream body must not contain %s\nbody: %s", bad, sent)
				}
			}
		})
	}
}
