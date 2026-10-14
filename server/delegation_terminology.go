package server

import (
	"encoding/json"
	"fmt"
	"slices"

	actool "github.com/Autumn-27/norma/tool"
)

// Exact shipped defaults only: edited descriptions, parameter defaults, bindings
// and enabled flags remain user-owned. Retaining old strings here is intentional.
var previousDelegationDescriptions = map[string][]string{
	"subagents": {
		"在后台启动独立子任务。立即返回 run_id，结束后结果自动回到当前对话。子任务继承当前会话实际使用的模型配置、全局资源和上下文约束；depends_on 可连接前后步骤。",
		"后台启动原生子任务，立即返回 run_id；结束或暂停时自动回传。子任务继承当前会话实际使用的模型配置。默认共享运行 cwd，显式 isolation=worktree 创建独立源码副本。",
	},
	"subagents_control": {
		"管理当前对话的后台任务：list/status/read/stop/resume。read 一次读取一个子任务，最多返回末尾 8000 字符；resume 保留成功结果，仅重试未完成部分。",
		"管理当前对话的后台任务。message 只向正在执行的子任务排队指令，在下次模型调用前送达；提前结束则丢弃。resume 可提交编辑后的 tasks，复用未变成功步骤；变更及其下游重跑。检查点必须给 checkpoint_id 和 response。read 一次一项，最多末尾 8000 字符。",
	},
	"spawn_task": {
		"新建一个子任务并启动探索引擎，返回 task_id。用于把一件事(如一道题/一个目标)派成独立任务。parent_ref 可选：填当前编排关联的父任务 id 做父子关联。",
	},
	"list_llm_profiles": {
		"列出可用的 LLM 配置(profile)：id、名称、模型、格式、是否为当前激活配置。用 id 给 spawn_task 的 llm_profile_id 参数指定子任务专属 LLM（如侦察用便宜模型、利用用强模型）。不含 API Key。",
	},
	"send_file": {
		"将已经生成的本地文件作为可下载附件发送给当前会话用户。文件先复制保存，源文件后续修改或工作树删除不影响下载。支持任意普通文件，单个不超过 128 MiB；目录请先打包。仅在主对话调用，子任务将文件路径交给主代理审查后发送。",
		"将已经生成的本地文件作为可下载附件发送给当前会话用户。文件先复制保存，源文件后续修改或工作树删除不影响下载。支持任意普通文件，大小上限以系统配置中的 Agent 文件发送设置为准；目录请先打包。仅在主对话调用，子任务将文件路径交给主代理审查后发送。",
	},
}

// Run after the structural workflow schema upgrade; do not rewrite schemas or
// editable prompts just to update terminology. CAS also protects concurrent edits.
func (s *Server) upgradeDelegationDescriptions() error {
	tools := append(s.workflowTools(), s.toolSpawnTask(), s.toolListLLMProfiles())
	tools = append(tools, s.chatFileTools()...)
	for _, tool := range tools {
		if err := s.upgradeDelegationDescription(tool); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) upgradeDelegationDescription(tool actool.CoreTool) error {
	row, err := s.m.pg.GetTool(tool.Name())
	if err != nil || row == nil || !row.System {
		return err
	}
	description := row.Description
	if slices.Contains(previousDelegationDescriptions[tool.Name()], description) {
		description = tool.Description()
	}
	schema := row.Schema
	if tool.Name() == "subagents_control" {
		var value map[string]any
		if err := json.Unmarshal(schema, &value); err != nil {
			return err
		}
		properties, _ := value["properties"].(map[string]any)
		label, _ := properties["label"].(map[string]any)
		if label["description"] == "read/message 的单个子任务" {
			label["description"] = "read/message 的单个子代理 label"
			schema, err = json.Marshal(value)
			if err != nil {
				return err
			}
		}
	}
	if description == row.Description && string(schema) == string(row.Schema) {
		return nil
	}
	result, err := s.m.pg.Exec(`UPDATE tools SET description=$2,schema=$3::jsonb
WHERE key=$1 AND system AND description=$4 AND schema=$5::jsonb`, row.Key, description, string(schema), row.Description, string(row.Schema))
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("tool %s was edited concurrently; terminology upgrade will retry", row.Key)
	}
	return nil
}
