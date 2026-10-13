package pibridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// Request-shape limits. The body size limit lives on the Relay; these bound the
// work a single (possibly buggy) client can make the relay do.
const (
	maxMessages = 20000
	maxTools    = 512
	maxStopSeqs = 16
	maxTemp     = 2.0
)

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,128}$`)

// apiError is an OpenAI-style error that knows its HTTP status.
type apiError struct {
	status int
	typ    string
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, a ...any) *apiError {
	return &apiError{status: 400, typ: "invalid_request_error", code: "invalid_request", msg: fmt.Sprintf(format, a...)}
}

// chatRequest is the subset of an OpenAI Chat Completions request the relay
// understands. Unknown fields (store, prompt_cache_key, parallel_tool_calls,
// …) are ignored on purpose: Pi sends provider-compat extras the relay has no
// use for.
type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	Tools         []chatTool      `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	Stop                json.RawMessage `json:"stop"`
	N                   *int            `json:"n"`
	ReasoningEffort     string          `json:"reasoning_effort"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	// Pi echoes a previous turn's reasoning back under whichever field the
	// upstream used when it streamed it; reasoning_details (see reasoning.go)
	// carries the signed thinking blocks.
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	ReasoningText    string          `json:"reasoning_text"`
	ReasoningDetails json.RawMessage `json:"reasoning_details"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func (c *chatRequest) includeUsage() bool {
	return c.StreamOptions != nil && c.StreamOptions.IncludeUsage
}

// completion converts the request into the provider-neutral llm request for
// model m, rejecting anything the llm types cannot represent faithfully.
func (c *chatRequest) completion(m Model) (llm.CompletionRequest, *apiError) {
	var req llm.CompletionRequest
	if len(c.Messages) == 0 {
		return req, badRequest("messages must be a non-empty array")
	}
	if len(c.Messages) > maxMessages {
		return req, badRequest("too many messages (max %d)", maxMessages)
	}
	if c.N != nil && *c.N != 1 {
		return req, badRequest("n must be 1")
	}
	if err := c.mapMessages(&req); err != nil {
		return req, err
	}
	tools, err := c.mapTools()
	if err != nil {
		return req, err
	}
	req.Tools = tools
	if err := c.mapSampling(&req, m); err != nil {
		return req, err
	}
	// The whole system prompt is the static prefix: let Anthropic put its cache
	// breakpoint after it (llm.buildAnthropicSystem). No effect on other formats.
	req.DynamicBoundary = len(req.System)
	return req, nil
}

// mapMessages fills req.System and req.Messages.
//
//   - system/developer messages (anywhere) become System segments in order;
//     llm has no mid-conversation system slot.
//   - consecutive tool messages become one user message of tool_result blocks;
//     image parts of a user or tool message become image blocks (a tool
//     message's land inside its tool_result).
//   - assistant reasoning becomes thinking blocks: signed ones restored from
//     reasoning_details, else one unsigned block from the plain reasoning field.
//   - assistant turns with neither text nor tool calls are dropped, like Pi does.
func (c *chatRequest) mapMessages(req *llm.CompletionRequest) *apiError {
	prevTool := false
	for i, msg := range c.Messages {
		where := fmt.Sprintf("messages[%d]", i)
		parts, err := parseContent(msg.Content, where+".content", msg.Role == "user" || msg.Role == "tool")
		if err != nil {
			return err
		}
		isTool := false
		switch msg.Role {
		case "system", "developer":
			if text := strings.Join(texts(parts), "\n"); strings.TrimSpace(text) != "" {
				req.System = append(req.System, text)
			}
		case "user":
			blocks := userBlocks(parts)
			if len(blocks) == 0 {
				return badRequest("%s: user message has no content", where)
			}
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Content: blocks})
		case "assistant":
			blocks, err := assistantBlocks(msg, texts(parts), where)
			if err != nil {
				return err
			}
			if blocks != nil {
				req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: blocks})
			}
		case "tool":
			isTool = true
			id := strings.TrimSpace(msg.ToolCallID)
			if id == "" {
				return badRequest("%s: tool message requires tool_call_id", where)
			}
			block := llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: id, Content: toolResultBlocks(parts)}
			if prevTool {
				last := &req.Messages[len(req.Messages)-1]
				last.Content = append(last.Content, block)
			} else {
				req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}})
			}
		default:
			return badRequest("%s: unsupported role %q", where, msg.Role)
		}
		prevTool = isTool
	}
	if len(req.Messages) == 0 {
		return badRequest("messages must contain at least one user, assistant or tool message")
	}
	return nil
}

// assistantBlocks builds the content of an assistant turn: thinking, text, then
// tool_use. It returns nil (not an error) for a turn with nothing replayable.
func assistantBlocks(msg chatMessage, parts []string, where string) ([]llm.ContentBlock, *apiError) {
	blocks := thinkingBlocks(msg)
	replayable := false
	for _, text := range parts {
		if text != "" {
			blocks = append(blocks, llm.TextBlock(text))
			replayable = true
		}
	}
	seen := map[string]bool{}
	for j, tc := range msg.ToolCalls {
		w := fmt.Sprintf("%s.tool_calls[%d]", where, j)
		if tc.Type != "" && tc.Type != "function" {
			return nil, badRequest("%s: unsupported tool call type %q", w, tc.Type)
		}
		if tc.Function == nil || !toolNameRe.MatchString(tc.Function.Name) {
			return nil, badRequest("%s: function.name is missing or invalid", w)
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" || seen[id] {
			return nil, badRequest("%s: id must be non-empty and unique within the message", w)
		}
		seen[id] = true
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if args[0] != '{' || !json.Valid([]byte(args)) {
			return nil, badRequest("%s: function.arguments must be a JSON object string", w)
		}
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: tc.Function.Name, Input: json.RawMessage(args)})
		replayable = true
	}
	if !replayable {
		return nil, nil
	}
	return blocks, nil
}

// mapTools validates and converts function tools. tool_choice "none" validates
// the tools but sends none; "required" / a forced function cannot be expressed
// in llm.CompletionRequest and is rejected rather than silently ignored.
func (c *chatRequest) mapTools() ([]llm.ToolSchema, *apiError) {
	if len(c.Tools) > maxTools {
		return nil, badRequest("too many tools (max %d)", maxTools)
	}
	out := make([]llm.ToolSchema, 0, len(c.Tools))
	seen := map[string]bool{}
	for i, t := range c.Tools {
		w := fmt.Sprintf("tools[%d]", i)
		if t.Type != "function" || t.Function == nil {
			return nil, badRequest("%s: only function tools are supported", w)
		}
		name := t.Function.Name
		if !toolNameRe.MatchString(name) {
			return nil, badRequest("%s: function.name %q is invalid", w, truncateRunes(name, 64))
		}
		if seen[name] {
			return nil, badRequest("%s: duplicate tool name %q", w, name)
		}
		seen[name] = true
		schema := t.Function.Parameters
		if schema == nil { // Anthropic rejects a null input_schema
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, llm.ToolSchema{Name: name, Description: t.Function.Description, InputSchema: schema})
	}
	switch choice := strings.TrimSpace(string(c.ToolChoice)); choice {
	case "", "null", `"auto"`:
		return out, nil
	case `"none"`:
		return nil, nil
	default:
		return nil, badRequest("tool_choice %s is not supported (only \"auto\" and \"none\")", truncateRunes(choice, 64))
	}
}

// mapSampling handles max tokens, temperature, stop and reasoning_effort.
//
// reasoning_effort is only honoured as an off switch ("none"/"off" forces
// thinking disabled for this request); effort levels are fixed by the
// provider's own config because llm.CompletionRequest has no effort field.
func (c *chatRequest) mapSampling(req *llm.CompletionRequest, m Model) *apiError {
	maxTokens := c.MaxCompletionTokens
	if maxTokens == nil {
		maxTokens = c.MaxTokens
	}
	if maxTokens != nil {
		if *maxTokens < 0 {
			return badRequest("max_tokens must not be negative")
		}
		req.MaxTokens = *maxTokens
		if m.MaxTokens > 0 && req.MaxTokens > m.MaxTokens {
			req.MaxTokens = m.MaxTokens
		}
	}
	if c.Temperature != nil {
		if *c.Temperature < 0 || *c.Temperature > maxTemp {
			return badRequest("temperature must be between 0 and %v", maxTemp)
		}
		t := *c.Temperature
		req.Temperature = &t
	}
	stop, err := parseStop(c.Stop)
	if err != nil {
		return err
	}
	req.Stop = stop
	switch strings.ToLower(strings.TrimSpace(c.ReasoningEffort)) {
	case "none", "off":
		req.Thinking = "disabled"
	}
	return nil
}

func parseStop(raw json.RawMessage) ([]string, *apiError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, badRequest("stop is invalid: %v", err)
		}
		list = []string{s}
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil, badRequest("stop must be a string or an array of strings")
	}
	if len(list) > maxStopSeqs {
		return nil, badRequest("too many stop sequences (max %d)", maxStopSeqs)
	}
	out := list[:0]
	for _, s := range list {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
