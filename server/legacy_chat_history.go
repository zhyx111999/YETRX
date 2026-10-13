package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

const nativeHistoryMarker = "native_history_import"

type legacyChatActivity struct {
	Kind, Tool, Summary, Detail string
	CreatedAt                   time.Time
}

// ensureNativeHistory is a one-time upgrade of conversations used by the old
// embedded runtime. The activity ledger is authoritative; previous tool calls
// are historical text, never instructions to execute again. The original files
// stay available for rollback. Call before saving a new human turn.
func (s *Server) ensureNativeHistory(id int64) error {
	sessionID := s.convBusyKey(id)
	legacy := filepath.Join(s.m.dir, "pi", "sessions", sessionID+".jsonl")
	if _, err := os.Stat(legacy); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	path := store.MainPath(sessionID)
	if file, err := os.Open(path); err == nil {
		var first transcript.Record
		decodeErr := json.NewDecoder(file).Decode(&first)
		file.Close()
		if decodeErr != nil && decodeErr != io.EOF {
			return decodeErr
		}
		if first.Type == nativeHistoryMarker {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	rows, err := s.m.pg.Query(`SELECT COALESCE(kind,''), COALESCE(tool,''), COALESCE(summary,''), COALESCE(detail,''), created_at
FROM conversation_activities WHERE conversation_id=$1 AND kind IN ('user','text','tool_use','tool_result') ORDER BY id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	var activities []legacyChatActivity
	for rows.Next() {
		var item legacyChatActivity
		if err := rows.Scan(&item.Kind, &item.Tool, &item.Summary, &item.Detail, &item.CreatedAt); err != nil {
			return err
		}
		activities = append(activities, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	workspace, err := filepath.Abs(filepath.Join(s.m.dir, "sessions", sessionID))
	if err != nil {
		return err
	}
	records := nativeHistoryRecords(activities, sessionID, workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if original, err := os.ReadFile(path); err == nil {
		backup, err := os.OpenFile(path+".before-native", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_, writeErr := backup.Write(original)
			closeErr := backup.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
		} else if !os.IsExist(err) {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".native-history-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nativeHistoryRecords(activities []legacyChatActivity, sessionID, workspace string) []transcript.Record {
	records := []transcript.Record{{Type: nativeHistoryMarker, SessionID: sessionID, UUID: "native-import-" + sessionID}}
	for _, item := range activities {
		text := item.Detail
		if strings.TrimSpace(text) == "" {
			text = item.Summary
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := llm.RoleAssistant
		switch item.Kind {
		case "user":
			role = llm.RoleUser
			var saved struct {
				Text        string           `json:"text"`
				Attachments []chatAttachment `json:"attachments"`
			}
			if json.Unmarshal([]byte(text), &saved) == nil && (saved.Text != "" || len(saved.Attachments) > 0) {
				text = composeAgentMessage(saved.Text, saved.Attachments, workspace)
			}
		case "tool_use":
			text = fmt.Sprintf("[历史工具调用 %s；仅供参考，已执行过]\n%s", item.Tool, text)
		case "tool_result":
			text = fmt.Sprintf("[历史工具返回 %s]\n%s", item.Tool, text)
		case "text":
		default:
			continue
		}
		message := llm.Message{Role: role, Content: []llm.ContentBlock{llm.TextBlock(text)}}
		records = append(records, transcript.Record{Type: "message", SessionID: sessionID,
			UUID: fmt.Sprintf("native-import-%s-%d", sessionID, len(records)), ParentUUID: records[len(records)-1].UUID,
			Timestamp: item.CreatedAt.UTC().Format(time.RFC3339Nano), Message: &message})
	}
	return records
}
