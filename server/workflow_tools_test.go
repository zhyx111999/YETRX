package server

import (
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestSubagentsUsesCurrentSessionModelSchema(t *testing.T) {
	var subagents actool.CoreTool
	for _, tool := range (&Server{}).workflowTools() {
		if tool.Name() == "subagents" {
			subagents = tool
			break
		}
	}
	if subagents == nil {
		t.Fatal("subagents tool is not registered")
	}
	if got := subagents.InputSchema()["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any); got["llm_profile_id"] != nil {
		t.Fatal("subagents still exposes a per-task llm_profile_id override")
	}
	if want := "当前会话实际使用的模型配置"; !strings.Contains(subagents.Description(), want) {
		t.Fatalf("description=%q, want it to explain current-session model inheritance", subagents.Description())
	}
}
