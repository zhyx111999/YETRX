package db

import (
	"database/sql"
	"fmt"

	"github.com/Autumn-27/artex/minimalprompt"
)

const nativeAgentMigration = "native_agent_v1"

// migrateDefaultAgent runs before seeding and commits the key, prompt, references
// and completion marker together. The marker is distinct from the old pi migration,
// whose existing agent key alone cannot tell us whether native migration has run.
func (d *DB) migrateDefaultAgent() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Cover concurrent migrations and inserts of absent keys. EXCLUSIVE also
	// serializes with SavePrompt's SELECT FOR UPDATE before either writes, while
	// ordinary readers can continue. Do not re-acquire Open's session schema lock.
	if _, err := tx.Exec(`LOCK TABLE agents IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var legacyID, agentID int64
	var legacyBuiltin, agentBuiltin bool
	if err := tx.QueryRow(`SELECT id, builtin FROM agents WHERE key='auto'`).Scan(&legacyID, &legacyBuiltin); err != nil && err != sql.ErrNoRows {
		return err
	}
	if err := tx.QueryRow(`SELECT id, builtin FROM agents WHERE key=$1`, minimalprompt.AgentKey).Scan(&agentID, &agentBuiltin); err != nil && err != sql.ErrNoRows {
		return err
	}
	if agentID != 0 && !agentBuiltin {
		return fmt.Errorf("agent key conflict: custom agent (id=%d) already uses key agent; rename it before retrying", agentID)
	}
	if agentID != 0 && legacyBuiltin {
		return fmt.Errorf("agent key conflict: builtin auto (id=%d) and agent (id=%d) both exist; resolve without discarding either history", legacyID, agentID)
	}
	var completed string
	if err := tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, nativeAgentMigration).Scan(&completed); err != nil && err != sql.ErrNoRows {
		return err
	}
	if completed == "done" {
		return tx.Commit()
	}

	if legacyBuiltin {
		agentID = legacyID
		// Preserve custom budgets; only the upstream Auto time default changes.
		_, err = tx.Exec(`UPDATE agents SET key=$1,
run_seconds=CASE WHEN run_seconds=1200 THEN 0 ELSE run_seconds END WHERE id=$2`, minimalprompt.AgentKey, agentID)
	} else if agentID == 0 {
		err = tx.QueryRow(`INSERT INTO agents(key, name, description, role, builtin, max_turns, run_seconds)
VALUES ($1, $2, $3, 'assistant', true, 0, 0) RETURNING id`,
			minimalprompt.AgentKey, minimalprompt.AgentName, minimalprompt.AgentDescription).Scan(&agentID)
	}
	if err != nil {
		return err
	}
	// Built-in identity changes once. Keep edited metadata and all resource,
	// model, enabled, shell and web-search settings, including pi-disabled rows.
	if _, err := tx.Exec(`UPDATE agents SET
name=CASE WHEN name IN ('Auto','Agent') THEN $1 ELSE name END,
description=CASE WHEN description IN ($2,$3) THEN $4 ELSE description END,
role='assistant' WHERE id=$5`, minimalprompt.AgentName,
		"平台操作助手：用工具管理任务(建/看/暂停/给提示)与资产，并可创建/修改 skill、自定义工具、MCP。",
		"通用对话助手，使用 pi 原生工具与 workflow，按需读取 skills。",
		minimalprompt.AgentDescription, agentID); err != nil {
		return err
	}

	var current sql.NullInt64
	var template, author sql.NullString
	if err := tx.QueryRow(`SELECT a.current_prompt_id, p.template_text, p.updated_by
FROM agents a LEFT JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, agentID).Scan(&current, &template, &author); err != nil {
		return err
	}
	if !current.Valid || (author.String == "system" && (template.String == legacyAutoPrompt || template.String == legacyPiPrompt)) {
		if _, err := savePrompt(tx, agentID, minimalprompt.Default, "Agent: native tools", "system"); err != nil {
			return err
		}
	}

	// A custom auto keeps its own conversations and bindings. Otherwise migrate
	// old key references too, including those left behind by the pi deployment.
	if legacyID == 0 || legacyBuiltin {
		for _, query := range []string{
			`UPDATE conversations SET agent_key='agent' WHERE agent_key='auto'`,
			`UPDATE agent_triggers SET agent_key='agent' WHERE agent_key='auto'`,
			`UPDATE tools SET agents=(SELECT jsonb_agg(
CASE WHEN value='"auto"'::jsonb THEN '"agent"'::jsonb ELSE value END ORDER BY ord)
FROM jsonb_array_elements(agents) WITH ORDINALITY AS binding(value, ord))
WHERE agents @> '["auto"]'::jsonb`,
		} {
			if _, err := tx.Exec(query); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES ($1, 'done')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, nativeAgentMigration); err != nil {
		return err
	}
	return tx.Commit()
}

// Exact former defaults, recognized only when saved by the system.
// Edited prompts stay current; all versions remain in agent_prompts.
const legacyAutoPrompt = `你是 **Auto**，这个渗透测试平台的「操作助手」。你不亲自渗透，而是**用工具操作平台**、按用户指令把事情办好。

你能做的（取决于给你开放了哪些工具）：
1. **任务操作**：list_tasks 看全局、spawn_task 起子任务、get_task_graph / list_task_findings 读某任务的进展与漏洞(含 flag)、get_task_worker_trace 看某个 work 的执行过程、pause_task 暂停、add_task_hint 给任务注入提示。
2. **平台管理**：create_skill / update_skill 建改技能；create_custom_tool / update_custom_tool 建改自定义工具(command/script/http)；create_mcp / update_mcp 建改 MCP 服务器。

原则：
- 先看清现状(list_tasks / get_task_graph 等)再动手；一步到位、少空转。
- 建/改 skill、工具、MCP 时，把用户意图翻译成正确的结构化参数(kind/exec/schema 等)，字段拿不准就按最小可用填。
- 用人话简洁汇报你做了什么、结果如何；只根据工具真实返回作答，不臆造。
- 只在授权范围内操作。`

const legacyPiPrompt = `You work inside the user's workspace and act through the tools below. Do what was asked, check results before reporting them, and say plainly what you did and what you could not do.

Tools:
- read: read a file (text or image).
- bash: run a shell command in the working directory.
- edit: replace exact text in an existing file.
- write: create or overwrite a file.
- grep: search file contents (when enabled).
- find: find files by name pattern (when enabled).
- ls: list a directory (when enabled).
- subagents: start background subagents; it returns a run id at once and the result is delivered back into this conversation when the run finishes.
- subagents_control: check, read, message, stop or resume a background run.

Skills listed below are loaded on demand: read a skill's SKILL.md when its description matches the task.`
