package server

import (
	"encoding/json"
	"fmt"
)

const workflowSchemaVersion = "native_workflow_controls_schema_v2"

// The UI permits editing descriptions/defaults, bindings and enabled flags.
// Upgrade the code-owned schema structure while retaining those user choices.
// Compare-and-swap prevents a concurrent page edit from being overwritten.
func (s *Server) upgradeWorkflowSchemas() error {
	if value, _, err := s.m.pg.GetSetting(workflowSchemaVersion); err != nil {
		return err
	} else if value == "true" {
		return nil
	}
	legacy := map[string]string{
		"subagents":         "在后台启动独立子任务。立即返回 run_id，结束后结果自动回到当前对话。子任务继承当前会话实际使用的模型配置、全局资源和上下文约束；depends_on 可连接前后步骤。",
		"subagents_control": "管理当前对话的后台任务：list/status/read/stop/resume。read 一次读取一个子任务，最多返回末尾 8000 字符；resume 保留成功结果，仅重试未完成部分。",
	}
	for _, tool := range s.workflowTools() {
		row, err := s.m.pg.GetTool(tool.Name())
		if err != nil {
			return err
		}
		if row == nil || !row.System {
			continue
		}
		var old map[string]any
		if err = json.Unmarshal(row.Schema, &old); err != nil {
			return err
		}
		schema := tool.InputSchema()
		preserveWorkflowSchemaEdits(schema, old)
		raw, err := json.Marshal(schema)
		if err != nil {
			return err
		}
		description := row.Description
		if description == legacy[tool.Name()] {
			description = tool.Description()
		}
		result, err := s.m.pg.Exec(`UPDATE tools SET schema=$2::jsonb,description=$3,updated_at=now()
WHERE key=$1 AND system AND schema=$4::jsonb AND description=$5`, tool.Name(), string(raw), description, string(row.Schema), row.Description)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("workflow tool %s was edited concurrently; schema upgrade will retry", tool.Name())
		}
	}
	return s.m.pg.SetSetting(workflowSchemaVersion, "true")
}

func preserveWorkflowSchemaEdits(next, previous map[string]any) {
	for _, name := range []string{"description", "default"} {
		if value, ok := previous[name]; ok {
			next[name] = value
		}
	}
	if properties, ok := next["properties"].(map[string]any); ok {
		old, _ := previous["properties"].(map[string]any)
		for key, value := range properties {
			dst, dok := value.(map[string]any)
			src, sok := old[key].(map[string]any)
			if dok && sok {
				preserveWorkflowSchemaEdits(dst, src)
			}
		}
	}
	if items, ok := next["items"].(map[string]any); ok {
		if old, ok := previous["items"].(map[string]any); ok {
			preserveWorkflowSchemaEdits(items, old)
		}
	}
}
