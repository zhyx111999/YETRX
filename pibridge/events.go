package pibridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

// EventRecorder translates pi's completed messages and tool lifecycle into the
// existing activity format. Incremental SDK snapshots are not stored repeatedly.
type EventRecorder struct {
	emit    func(db.Activity)
	running bool
	failed  bool
	usage   piUsage
}

type piUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

type piContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Usage        piUsage         `json:"usage"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	CustomType   string          `json:"customType"`
	Display      *bool           `json:"display"`
}

func NewEventRecorder(emit func(db.Activity)) *EventRecorder {
	return &EventRecorder{emit: emit}
}

func (r *EventRecorder) record(activity db.Activity) {
	activity.Worker = "agent"
	if r.emit != nil {
		r.emit(activity)
	}
}

func (r *EventRecorder) Handle(raw json.RawMessage) {
	var event struct {
		Type       string          `json:"type"`
		Message    piMessage       `json:"message"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Args       json.RawMessage `json:"args"`
		Result     struct {
			Content json.RawMessage `json:"content"`
			Details json.RawMessage `json:"details"`
		} `json:"result"`
		IsError      bool   `json:"isError"`
		Aborted      bool   `json:"aborted"`
		Error        string `json:"error"`
		ErrorMessage string `json:"errorMessage"`
		Attempt      int    `json:"attempt"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return
	}
	switch event.Type {
	case "agent_start":
		if !r.running {
			r.running, r.failed, r.usage = true, false, piUsage{}
		}
	case "message_end":
		message := event.Message
		if message.Role == "assistant" {
			r.usage.Input += message.Usage.Input
			r.usage.Output += message.Usage.Output
			r.usage.CacheRead += message.Usage.CacheRead
			r.usage.CacheWrite += message.Usage.CacheWrite
			for _, block := range contentBlocks(message.Content) {
				switch block.Type {
				case "text":
					if block.Text != "" {
						r.record(db.Activity{Kind: "text", Summary: preview(block.Text, 400), Detail: block.Text})
					}
				case "thinking":
					if block.Thinking != "" {
						r.record(db.Activity{Kind: "thinking", Summary: preview(block.Thinking, 400), Detail: block.Thinking})
					}
				}
			}
			r.failed = message.StopReason == "error"
			if r.failed {
				text := message.ErrorMessage
				if text == "" {
					text = "模型请求失败"
				}
				r.record(db.Activity{Kind: "text", IsError: true, Summary: preview(text, 400), Detail: text})
			}
		} else if message.Role == "custom" && (message.Display == nil || *message.Display) {
			text := contentText(message.Content)
			if text != "" {
				r.record(db.Activity{Kind: "text", Summary: preview(text, 400), Detail: text})
			}
		}
	case "tool_execution_start":
		input := string(event.Args)
		if input == "" {
			input = "{}"
		}
		r.record(db.Activity{Kind: "tool_use", Tool: event.ToolName, ToolUseID: event.ToolCallID,
			Summary: preview(event.ToolName+" "+input, 400), Detail: input})
	case "tool_execution_end":
		text := contentText(event.Result.Content)
		if len(event.Result.Details) > 0 && string(event.Result.Details) != "null" {
			text += "\n\n" + string(event.Result.Details)
		}
		r.record(db.Activity{Kind: "tool_result", Tool: event.ToolName, ToolUseID: event.ToolCallID,
			IsError: event.IsError, Summary: preview(text, 400), Detail: text})
	case "compaction_start":
		r.record(db.Activity{Kind: "compaction", Summary: "正在压缩会话上下文", Detail: "pi 正在保留关键上下文并整理会话历史。"})
	case "compaction_end":
		if event.ErrorMessage != "" {
			r.record(db.Activity{Kind: "compaction", IsError: true, Summary: "上下文压缩失败", Detail: event.ErrorMessage})
		}
	case "auto_retry_start":
		r.record(db.Activity{Kind: "text", Summary: fmt.Sprintf("模型请求重试（第 %d 次）", event.Attempt), Detail: event.ErrorMessage})
	case "runtime_error", "extension_error":
		r.failed = true
		r.record(db.Activity{Kind: "result", IsError: true, Summary: preview(event.Error, 400), Detail: event.Error})
	case "agent_settled":
		label := "完成"
		if event.Aborted {
			label = "已停止"
		} else if r.failed {
			label = "执行出错"
		}
		usage := r.usage
		inputTokens := usage.Input + usage.CacheRead + usage.CacheWrite
		r.record(db.Activity{Kind: "result", IsError: r.failed && !event.Aborted, Summary: label, Detail: label,
			InputTokens: &inputTokens, OutputTokens: &usage.Output,
			CacheReadTokens: &usage.CacheRead, CacheWriteTokens: &usage.CacheWrite})
		r.running, r.failed, r.usage = false, false, piUsage{}
	}
}

func contentBlocks(raw json.RawMessage) []piContent {
	var blocks []piContent
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []piContent{{Type: "text", Text: text}}
	}
	return nil
}

func contentText(raw json.RawMessage) string {
	var pieces []string
	for _, block := range contentBlocks(raw) {
		if block.Text != "" {
			pieces = append(pieces, block.Text)
		} else if block.Type == "image" {
			pieces = append(pieces, "[图片已发送给模型]")
		}
	}
	return strings.Join(pieces, "\n")
}

func preview(text string, limit int) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}
