package llm

import (
	"encoding/json"
	"testing"
)

const (
	pngDataURL = "data:image/png;base64,iVBOR"
	jpgDataURL = "data:image/jpeg;base64,/9j/4"
)

// imageMsgs is one user turn with text + a base64 image + a URL image, preceded
// by a tool round whose result carries text and an image.
func imageMsgs() []Message {
	return []Message{
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockToolUse, ID: "c1", Name: "read", Input: json.RawMessage(`{}`)}}},
		{Role: RoleUser, Content: []ContentBlock{
			{Type: BlockToolResult, ToolUseID: "c1", Content: []ContentBlock{TextBlock("shot"), ImageBase64Block("image/jpeg", "/9j/4")}},
			TextBlock("look"),
			ImageBase64Block("image/png", "iVBOR"),
			ImageURLBlock("https://example.com/a.png"),
		}},
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnthropicBodyKeepsImagesNative(t *testing.T) {
	p := &anthropicProvider{cfg: Config{Model: "m"}}
	raw, err := p.buildBody(CompletionRequest{Messages: imageMsgs()}, false)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Messages) != 2 {
		t.Fatalf("body: %v %s", err, raw)
	}
	const want = `{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"shot"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4"}}]},` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}`
	if got := string(body.Messages[1]); got != want {
		t.Errorf("anthropic user message\n got: %s\nwant: %s", got, want)
	}
}

func TestOpenAIMessagesEncodeImages(t *testing.T) {
	got := jsonOf(t, toOpenAIMessages("", imageMsgs()))
	const want = `[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
		// tool message keeps the text; its image rides in the user message right after the tool group
		`{"role":"tool","content":"shot","tool_call_id":"c1"},` +
		`{"role":"user","content":[{"type":"text","text":"Attached image(s) from tool result:"},{"type":"image_url","image_url":{"url":"` + jpgDataURL + `"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + pngDataURL + `"}},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]`
	if got != want {
		t.Errorf("openai messages\n got: %s\nwant: %s", got, want)
	}
}

func TestOpenAIMessagesImageOnlyToolResultAndNilSource(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{
		{Type: BlockToolResult, ToolUseID: "c1", Content: []ContentBlock{ImageBase64Block("image/png", "iVBOR")}},
		{Type: BlockImage}, // no source: dropped, never serialized as an empty part
	}}}
	oa := toOpenAIMessages("", msgs)
	if len(oa) != 2 || oa[0].Role != "tool" || oa[0].Content != "(see attached image)" || oa[1].Role != "user" || len(oa[1].Parts) != 2 {
		t.Fatalf("unexpected messages: %s", jsonOf(t, oa))
	}
}

func TestOpenAIMessagesTextOnlyStaysPlainString(t *testing.T) {
	got := jsonOf(t, toOpenAIMessages("", []Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock("a"), TextBlock("b")}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("c1", "out", false)}},
	}))
	const want = `[{"role":"user","content":"ab"},{"role":"tool","content":"out","tool_call_id":"c1"}]`
	if got != want {
		t.Errorf("text-only encoding changed\n got: %s\nwant: %s", got, want)
	}
}

func TestResponsesInputEncodesImages(t *testing.T) {
	got := jsonOf(t, toResponsesInput(imageMsgs()))
	const want = `[{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"shot"},` +
		`{"role":"user","content":[{"type":"input_text","text":"Attached image(s) from tool result:"},{"type":"input_image","image_url":"` + jpgDataURL + `","detail":"auto"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"` + pngDataURL + `","detail":"auto"},{"type":"input_image","image_url":"https://example.com/a.png","detail":"auto"}]}]`
	if got != want {
		t.Errorf("responses input\n got: %s\nwant: %s", got, want)
	}
}

func TestResponsesInputTextOnlyUnchangedAndToolImagesFlushInOrder(t *testing.T) {
	got := jsonOf(t, toResponsesInput([]Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock("a"), TextBlock("b")}},
		{Role: RoleUser, Content: []ContentBlock{
			{Type: BlockToolResult, ToolUseID: "c1", Content: []ContentBlock{ImageURLBlock("https://example.com/x.png")}},
			TextBlock("after"),
		}},
	}))
	const want = `[{"role":"user","content":"ab"},` +
		`{"type":"function_call_output","call_id":"c1","output":"(see attached image)"},` +
		`{"role":"user","content":[{"type":"input_text","text":"Attached image(s) from tool result:"},{"type":"input_image","image_url":"https://example.com/x.png","detail":"auto"}]},` +
		`{"role":"user","content":"after"}]`
	if got != want {
		t.Errorf("responses input\n got: %s\nwant: %s", got, want)
	}
}
