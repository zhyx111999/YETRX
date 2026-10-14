package server

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
)

func TestDelegationDescriptionUpgradePreservesUserChoices(t *testing.T) {
	e := newNativeEnv(t)
	s := &Server{m: &Manager{pg: e.pg}}
	tools := append(s.workflowTools(), s.toolSpawnTask(), s.toolListLLMProfiles())
	tools = append(tools, s.chatFileTools()...)
	original := map[string]*db.Tool{}
	for _, tool := range tools {
		row, err := e.pg.GetTool(tool.Name())
		if err != nil {
			t.Fatal(err)
		}
		original[tool.Name()] = row
	}
	t.Cleanup(func() {
		for key, row := range original {
			if row == nil {
				_, _ = e.pg.Exec(`DELETE FROM tools WHERE key=$1`, key)
			} else {
				_ = e.pg.UpdateTool(key, row.Description, row.Schema, mustJSON(row.Agents), row.Enabled)
			}
		}
	})
	for _, tool := range tools {
		key := tool.Name()
		schema := tool.InputSchema()
		props := schema["properties"].(map[string]any)
		props["custom"] = map[string]any{"type": "string", "description": "用户说明：子任务", "default": "KEEP"}
		if key == "subagents_control" {
			label := props["label"].(map[string]any)
			label["description"], label["default"] = "read/message 的单个子任务", "my-label"
		}
		raw := mustJSON(schema)
		if err := e.pg.SeedTool(key, tool.Description(), raw, mustJSON([]string{e.keys[1]})); err != nil {
			t.Fatal(err)
		}
		for index, old := range previousDelegationDescriptions[key] {
			oldLabel := "read/message 的单个子任务"
			if key == "subagents_control" {
				if index == 0 {
					oldLabel = "read 指定一个子任务"
				}
				props["label"].(map[string]any)["description"] = oldLabel
				raw = mustJSON(schema)
			}
			if err := e.pg.UpdateTool(key, old, raw, mustJSON([]string{e.keys[1]}), false); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.upgradeDelegationDescriptions(); err != nil {
					t.Fatal(err)
				}
			}
			row, err := e.pg.GetTool(key)
			if err != nil {
				t.Fatal(err)
			}
			if row.Description != tool.Description() || row.Enabled || !slices.Equal(row.Agents, []string{e.keys[1]}) {
				t.Fatalf("%s description/preferences mismatch: %+v", key, row)
			}
			var got map[string]any
			if err := json.Unmarshal(row.Schema, &got); err != nil {
				t.Fatal(err)
			}
			if key == "subagents_control" {
				gotLabel := got["properties"].(map[string]any)["label"].(map[string]any)
				if gotLabel["description"] != "read/message 的单个子代理 label" {
					t.Fatal("legacy label not upgraded")
				}
				gotLabel["description"] = oldLabel
			}
			var want map[string]any
			_ = json.Unmarshal(raw, &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s schema edits lost", key)
			}
		}
		if key == "subagents_control" {
			props["label"].(map[string]any)["description"] = "用户自定义 label"
		}
		raw = mustJSON(schema)
		if err := e.pg.UpdateTool(key, "用户自定义子任务说明", raw, mustJSON([]string{e.keys[1]}), false); err != nil {
			t.Fatal(err)
		}
		if err := s.upgradeDelegationDescriptions(); err != nil {
			t.Fatal(err)
		}
		row, _ := e.pg.GetTool(key)
		var got, want any
		_ = json.Unmarshal(row.Schema, &got)
		_ = json.Unmarshal(raw, &want)
		if row.Description != "用户自定义子任务说明" || !reflect.DeepEqual(got, want) || row.Enabled || !slices.Equal(row.Agents, []string{e.keys[1]}) {
			t.Fatalf("%s custom preferences overwritten", key)
		}
	}
}

func TestDelegationToolsAndReportIdentifyMechanism(t *testing.T) {
	s := &Server{}
	for _, tool := range s.workflowTools() {
		if !strings.Contains(tool.Description(), "子代理") || !strings.Contains(tool.Description(), "探索") {
			t.Fatal(tool.Name())
		}
	}
	if text := s.toolSpawnTask().Description(); !strings.Contains(text, "探索子任务") || !strings.Contains(text, "持续规划") {
		t.Fatal(text)
	}
	for _, status := range []workflow.Status{workflow.Succeeded, workflow.Paused, workflow.Failed} {
		text := workflowReport(workflow.Run{ID: "wf-fixture", Status: status})
		if !strings.HasPrefix(text, "子代理运行 wf-fixture：") || strings.Contains(text, "后台子任务") {
			t.Fatal(text)
		}
	}
}
