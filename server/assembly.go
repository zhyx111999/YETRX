package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

// Every preset sees the same enabled resources. Skills still unlock their MCP
// tools on demand; bindings remain stored but no longer restrict discovery.
func wireAgentAugment(pg *db.DB, skillDir string, hostTools func() ([]actool.CoreTool, map[string][]string)) {
	agent.ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		a, err := pg.GetAgentByKey(agentKey)
		if err != nil {
			return nil, agent.DeferredInfo{Err: fmt.Errorf("preset lookup failed: %w", err)}, nil
		}
		if a == nil {
			return nil, agent.DeferredInfo{}, nil
		}
		var extra []actool.CoreTool

		reg, err := loadSharedSkills(ctx, pg, skillDir)
		if err != nil {
			return nil, agent.DeferredInfo{Err: fmt.Errorf("shared skill settings unavailable: %w", err)}, nil
		}
		// A server named by any enabled skill's `mcps:` is skill-gated: its tools
		// are deferred + locked (not in the global block) until that skill loads.
		gated := map[string]bool{}
		if reg != nil {
			for _, s := range reg.List() {
				for _, srv := range s.MCPs {
					gated[srv] = true
				}
			}
		}

		// Connect globally enabled MCPs. Skill-referenced tools stay locked until
		// the skill loads; all other enabled MCP tools are initially discoverable.
		var closers []io.Closer
		cleanup := func() {
			for _, c := range closers {
				_ = c.Close()
			}
		}
		setupError := func(name string, err error) ([]actool.CoreTool, agent.DeferredInfo, func()) {
			return nil, agent.DeferredInfo{Err: fmt.Errorf("MCP %s unavailable: %w", name, err)}, cleanup
		}
		serverTools := map[string][]string{} // server name → its tool names
		var allNames, globalNames []string
		globalSet := map[string]bool{}
		{
			all, err := pg.ListMCP()
			if err != nil {
				return setupError("catalog", err)
			}
			for _, m := range all {
				if !m.Enabled {
					continue
				}
				skillGated := gated[m.Name]
				cl, err := connectMCP(ctx, m)
				if err != nil {
					return setupError(m.Name, err)
				}
				closers = append(closers, cl)
				ts, err := cl.Tools(ctx)
				if err != nil {
					return setupError(m.Name, err)
				}
				for _, t := range ts {
					extra = append(extra, t)
					allNames = append(allNames, t.Name())
					serverTools[m.Name] = append(serverTools[m.Name], t.Name())
					if !skillGated {
						globalNames = append(globalNames, t.Name())
						globalSet[t.Name()] = true
					}
					// skill-gated tools stay out of globalNames; unlocked via unlockSkill()
				}
			}
		}

		// Shared call-gate: global MCP tools are unlocked from the start; skill-gated
		// ones are unlocked when their skill loads (or replayed from history — C2).
		unlock := actool.NewUnlockSet(globalNames...)
		unlockSkill := func(skillName string) {
			if reg == nil {
				return
			}
			if s, ok := reg.Get(skillName); ok {
				for _, srv := range s.MCPs {
					unlock.Add(serverTools[srv]...)
				}
			}
		}

		// Skill meta-tool: on load, unlock the skill's MCPs and reveal their names
		// (the ones not already in the global block).
		if reg != nil {
			reg.OnInvoke = func(s skill.Skill) string {
				unlockSkill(s.Name)
				var reveal []string
				for _, srv := range s.MCPs {
					for _, n := range serverTools[srv] {
						if !globalSet[n] {
							reveal = append(reveal, n)
						}
					}
				}
				return actool.RenderDeferredToolsBlock(reveal)
			}
			// Attribution for the usage ledger: ToolAugment only gets (ctx, agentKey),
			// so the run's task/session ids ride in on the ctx (agent.RunInfo). Read it
			// once here — this closure is rebuilt per run, so the captured value always
			// belongs to this run.
			extra = append(extra, meterSkillTool(reg.Tool(), pg, reg, a.Key, agent.RunInfoFrom(ctx)))
		}

		// Enabled host tools share the same discovery path. Deferred schemas stay
		// behind SearchExtraTools/ExecuteExtraTool without preset-specific gates.
		if hostTools != nil {
			ht, deferredBinds := hostTools()
			extra = append(extra, ht...)
			for name := range deferredBinds {
				allNames = append(allNames, name)       // schema withheld from the prompt
				globalNames = append(globalNames, name) // advertised in the deferred block
				globalSet[name] = true
				if unlock != nil {
					unlock.Add(name) // global deferred → callable from the start
				}
			}
		}

		def := agent.DeferredInfo{
			Deferred:    allNames,
			GlobalNames: globalNames,
			Unlock:      unlock,
			UnlockSkill: unlockSkill,
		}
		return extra, def, cleanup
	}
}

// seedPrompts writes each built-in agent's code-default prompt body into
// agent_prompts on startup — first-insert only (SeedPromptIfEmpty is a no-op once
// any version exists), so the DB becomes the authoritative editable source while
// user edits survive restarts. Runs after seedBuiltins has created the agent rows.
func seedPrompts(pg *db.DB) {
	for key, tmpl := range agent.BuiltinPromptSeeds() {
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			log.Printf("[prompts] seed %s 跳过: agent 不存在 (%v)", key, err)
			continue
		}
		if err := pg.SeedPromptIfEmpty(a.ID, tmpl); err != nil {
			log.Printf("[prompts] seed %s 失败: %v", key, err)
		}
	}
}

// wireTools seeds the global catalog without overwriting user edits, then applies
// enabled states, descriptions, schemas and parameter defaults to every role.
func wireTools(pg *db.DB, domainReg map[string]actool.CoreTool) {
	agent.FindingTrafficBindingEnabled = func() bool { return pg.GetBool(settingAgentTrafficBinding, false) }
	// Seed the built-in domain tools (first-insert only; DO NOTHING preserves edits).
	// No startup prune: rows we didn't seed are left alone so future user-defined
	// custom tools (system=false, added via the UI) survive restarts.
	for _, s := range agent.BuiltinToolSeeds() {
		schema, _ := json.Marshal(s.Schema)
		agents, _ := json.Marshal(s.Agents)
		if err := pg.SeedTool(s.Key, s.Desc, schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", s.Key, err)
		}
	}
	// Seed traffic metadata without rewriting historical bindings. Runtime
	// availability still requires the existing global capture switch.
	trafficAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range traffic.SeedToolMetas() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := pg.SeedTool(t.Name(), t.Description(), schema, trafficAgents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Name(), err)
		}
	}
	// bashInteractiveShellNote is appended to Bash's description ONLY for agents whose
	// interactive_shell is on, so Bash points at shell_open for interactive programs
	// without ever referencing a tool that isn't injected (§14.1/§14.2).
	const bashInteractiveShellNote = "\n\n需要【交互输入】的程序（msfconsole / ssh 交互登录 / mysql、psql、python 等 REPL / 密码或 yes/no 提示 / nc 反弹 shell）不要用 Bash（它没有 stdin、会卡住），改用 shell_open 开交互会话（用完 shell_close）。一次性、非交互命令仍用 Bash。"
	agent.ToolResolve = func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool {
		rows, err := pg.ListTools()
		if err != nil {
			log.Printf("[tools] read global enablement failed; tool assembly unavailable: %v", err)
			return []actool.CoreTool{}
		}
		byKey := make(map[string]*db.Tool, len(rows))
		for _, t := range rows {
			byKey[t.Key] = t
		}
		runInfo := agent.RunInfoFrom(ctx)
		resolve := func(t actool.CoreTool, row *db.Tool) actool.CoreTool {
			var schema map[string]any
			if len(row.Schema) > 0 {
				_ = json.Unmarshal(row.Schema, &schema)
			}
			return meterTool(agent.DecorateTool(t, row.Description, schema), pg, row.Key, agentKey, runInfo)
		}
		out := tools[:0:0]
		for _, t := range tools {
			row, known := byKey[t.Name()]
			if !known { // MCP/skill/host tool: no row → untouched
				out = append(out, t)
				continue
			}
			if !row.Enabled {
				continue
			}
			out = append(out, resolve(t, row))
		}
		// Fill absent globally available domain operations. A per-task instance
		// always wins over its global counterpart, preserving its real stores.
		if len(domainReg) > 0 {
			inList := make(map[string]bool, len(tools))
			for _, t := range tools {
				inList[t.Name()] = true
			}
			for _, row := range rows {
				if row.Kind == "shell" || !row.Enabled || inList[row.Key] {
					continue
				}
				inst, ok := domainReg[row.Key]
				if !ok {
					continue // not a domain tool; custom/host tools are injected via hostTools()
				}
				out = append(out, resolve(inst, row))
			}
		}
		// Shell declarations are global environment hints, not callable tools.
		var shellHints []string
		for _, row := range rows {
			if row.Kind == "shell" && row.Enabled {
				shellHints = append(shellHints, "- "+row.Key+": "+row.Description)
			}
		}
		if len(shellHints) > 0 {
			note := "\n\n以下工具已安装在此 bash 环境中，可直接通过 Bash 调用：\n" + strings.Join(shellHints, "\n")
			for i, t := range out {
				if t.Name() == "Bash" {
					out[i] = agent.DecorateTool(t, t.Description()+note, t.InputSchema())
					break
				}
			}
		}
		// interactive shell: gated purely by the agent's interactive_shell flag (like
		// web_search), NOT by tools-table binding. When on, inject the 5 shell_* tools
		// and COUPLE the Bash description addendum so it points at shell_open — and never
		// dangles when off. See docs/交互式shell设计.md §14.2.
		if !actool.InteractiveShellDisabled() {
			if a, err := pg.GetAgentByKey(agentKey); err == nil && a != nil && a.InteractiveShell {
				out = append(out, actool.ShellSessionTools()...)
				for i, t := range out {
					if t.Name() == "Bash" {
						out[i] = agent.DecorateTool(t, t.Description()+bashInteractiveShellNote, t.InputSchema())
						break
					}
				}
			}
		}
		return out
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// buildDomainReg supplies only operations with real global stores. Task-local
// operations are supplied by each execution's own ToolSet, before this registry.
func buildDomainReg(as *db.AssetStore) map[string]actool.CoreTool {
	if as == nil {
		return nil
	}
	serverTS := agent.NewToolSet(nil, "")
	serverTS.SetAssetStore(as, as.Companies())
	reg := make(map[string]actool.CoreTool)
	for _, t := range serverTS.AvailableDomainTools() {
		reg[t.Name()] = t
	}
	return reg
}

func jsonStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func jsonStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}
