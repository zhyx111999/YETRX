package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type piLegacyActivity struct {
	Kind      string
	Tool      string
	Summary   string
	Detail    string
	CreatedAt time.Time
}

// piConversationHistory is used only before the first pi session file is created.
// Old tool records remain contextual text: replaying their former tool calls would
// incorrectly invoke a different runtime or require unavailable provider signatures.
func (s *Server) piConversationHistory(conversationID int64, omitCurrentUser bool, modelID string) ([]json.RawMessage, error) {
	rows, err := s.m.pg.Query(`SELECT COALESCE(kind,''),COALESCE(tool,''),COALESCE(summary,''),COALESCE(detail,''),created_at
FROM conversation_activities WHERE conversation_id=$1 AND kind IN ('user','text','tool_use','tool_result') ORDER BY id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var activities []piLegacyActivity
	for rows.Next() {
		var item piLegacyActivity
		if err := rows.Scan(&item.Kind, &item.Tool, &item.Summary, &item.Detail, &item.CreatedAt); err != nil {
			return nil, err
		}
		activities = append(activities, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(filepath.Join(s.m.dir, "sessions", s.convBusyKey(conversationID)))
	if err != nil {
		return nil, err
	}
	return buildPiHistory(activities, omitCurrentUser, workspace, modelID)
}

func buildPiHistory(activities []piLegacyActivity, omitCurrentUser bool, workspace, modelID string) ([]json.RawMessage, error) {
	if omitCurrentUser && len(activities) > 0 && activities[len(activities)-1].Kind == "user" {
		activities = activities[:len(activities)-1]
	}
	var messages []map[string]any
	for _, item := range activities {
		text := item.Detail
		if strings.TrimSpace(text) == "" {
			text = item.Summary
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := "assistant"
		switch item.Kind {
		case "user":
			role = "user"
			var saved struct {
				Text        string           `json:"text"`
				Attachments []chatAttachment `json:"attachments"`
			}
			if json.Unmarshal([]byte(text), &saved) == nil && (saved.Text != "" || len(saved.Attachments) > 0) {
				text = composeAgentMessage(saved.Text, saved.Attachments, workspace)
			}
		case "tool_use":
			text = fmt.Sprintf("[升级前的工具调用记录：%s；仅作为历史上下文，不重新执行]\n%s", item.Tool, text)
		case "tool_result":
			text = fmt.Sprintf("[升级前的工具返回记录：%s]\n%s", item.Tool, text)
		case "text":
		default:
			continue
		}
		block := map[string]any{"type": "text", "text": text}
		if role == "assistant" && len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			last := messages[len(messages)-1]
			last["content"] = append(last["content"].([]map[string]any), block)
			continue
		}
		message := map[string]any{"role": role, "content": []map[string]any{block}, "timestamp": item.CreatedAt.UnixMilli()}
		if role == "assistant" {
			message["api"], message["provider"], message["model"] = "openai-completions", "yetrx", modelID
			message["stopReason"] = "stop"
			message["usage"] = map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
				"cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}
		}
		messages = append(messages, message)
	}
	out := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		encoded, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded)
	}
	return out, nil
}
