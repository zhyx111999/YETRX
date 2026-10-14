package server

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestWorkflowSchemaUpgradePreservesUserChoices(t *testing.T) {
	e := newNativeEnv(t)
	s := &Server{m: &Manager{pg: e.pg}}
	oldFlag, hadFlag, err := e.pg.GetSetting(workflowSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]*db.Tool{}
	for _, key := range []string{"subagents", "subagents_control"} {
		original[key], err = e.pg.GetTool(key)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for key, row := range original {
			if row == nil {
				_, _ = e.pg.Exec(`DELETE FROM tools WHERE key=$1`, key)
			} else {
				_ = e.pg.UpdateTool(key, row.Description, row.Schema, mustJSON(row.Agents), row.Enabled)
			}
		}
		if hadFlag {
			_ = e.pg.SetSetting(workflowSchemaVersion, oldFlag)
		} else {
			_, _ = e.pg.Exec(`DELETE FROM settings WHERE key=$1`, workflowSchemaVersion)
		}
	})
	for _, key := range []string{"subagents", "subagents_control"} {
		schema := json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["list","status","read","stop","resume"]},"tasks":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"task":{"type":"string","description":"用户自定义任务说明","default":"保留默认值"}},"required":["label","task"]}}},"required":["tasks"]}`)
		if err = e.pg.SeedTool(key, "custom", schema, mustJSON([]string{"agent"})); err != nil {
			t.Fatal(err)
		}
		if err = e.pg.UpdateTool(key, "用户自定义工具描述", schema, mustJSON([]string{e.keys[1]}), false); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = e.pg.Exec(`DELETE FROM settings WHERE key=$1`, workflowSchemaVersion)
	if err = s.upgradeWorkflowSchemas(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"subagents", "subagents_control"} {
		row, err := e.pg.GetTool(key)
		if err != nil {
			t.Fatal(err)
		}
		if row.Description != "用户自定义工具描述" || row.Enabled || !slices.Equal(row.Agents, []string{e.keys[1]}) {
			t.Fatalf("user choices changed: %+v", row)
		}
		var schema map[string]any
		if err = json.Unmarshal(row.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		props := schema["properties"].(map[string]any)
		taskSchema := props["tasks"].(map[string]any)["items"].(map[string]any)
		taskProps := taskSchema["properties"].(map[string]any)
		if taskProps["isolation"] == nil || taskProps["checkpoint"] == nil || taskProps["llm_profile_id"] != nil {
			t.Fatal("new schema not installed")
		}
		task := taskProps["task"].(map[string]any)
		if task["description"] != "用户自定义任务说明" || task["default"] != "保留默认值" {
			t.Fatal("parameter edits lost")
		}
		if key == "subagents_control" {
			actions := props["action"].(map[string]any)["enum"].([]any)
			found := false
			for _, value := range actions {
				found = found || value == "message"
			}
			if !found || props["response"] == nil {
				t.Fatal("control actions not upgraded")
			}
		}
		// An edit after migration remains untouched on the next startup.
		if err = e.pg.UpdateTool(key, "later edit", row.Schema, mustJSON(row.Agents), true); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.upgradeWorkflowSchemas(); err != nil {
		t.Fatal(err)
	}
	row, _ := e.pg.GetTool("subagents")
	if row.Description != "later edit" || !row.Enabled {
		t.Fatal("one-time migration overwrote later edit")
	}
}
