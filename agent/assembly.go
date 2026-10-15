package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"

	actool "github.com/Autumn-27/norma/tool"
)

// DeferredInfo carries the deferred-tools wiring an agent needs to build its
// Options: the MCP tool names whose schemas are withheld, the subset listed in the
// global system-prompt block (non-skill-gated), and the shared session unlock set.
// UnlockSkill unlocks a named skill's MCPs — hosts call it to rebuild the unlock set
// from history on a resumed session (design doc C2).
type DeferredInfo struct {
	Err             error             // resource setup failed; callers must stop before creating a session
	FindingGuidance string            // derived from the final permitted tools, including DB overrides
	Deferred        []string          // all MCP tool names (schema withheld)
	GlobalNames     []string          // MCP names to list in the system-prompt block
	Unlock          *actool.UnlockSet // shared call-gate; nil when no MCP tools
	UnlockSkill     func(skillName string)
}

// ToolAugment supplies globally enabled skills, MCPs and host capabilities.
// Deferred discovery and skill unlocks remain session-local. cleanup releases
// clients created for this execution. Nil keeps the built-in tool path.
var ToolAugment func(ctx context.Context, agentKey string) (extra []actool.CoreTool, def DeferredInfo, cleanup func())

// AugmentTools returns base plus the agent's visible skill/MCP tools, the
// DeferredInfo, and a cleanup func the caller must defer (closes MCP clients).
// Built-in base tools are kept as-is — never filtered (内置工具留代码层，不做可见性过滤).
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func()) {
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	if ToolAugment != nil {
		var extra []actool.CoreTool
		var cl func()
		extra, def, cl = ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if def.Err != nil {
			return nil, def, cleanup
		}
		if len(extra) > 0 {
			out = append(append([]actool.CoreTool{}, base...), extra...)
		}
	}
	// The global catalog controls enablement and parameter/description overrides.
	if ToolResolve != nil {
		out = ToolResolve(ctx, agentKey, out)
	}
	out, def.FindingGuidance = findingWorkflowTools(agentKey, out)
	seen := make(map[string]bool, len(out))
	unique := make([]actool.CoreTool, 0, len(out))
	for _, t := range out {
		if seen[t.Name()] {
			log.Printf("[tools] duplicate registration %q skipped; first execution-specific instance retained", t.Name())
			continue
		}
		seen[t.Name()] = true
		unique = append(unique, guardPanic(t))
	}
	keepNames := func(names []string) []string {
		kept := make([]string, 0, len(names))
		added := map[string]bool{}
		for _, name := range names {
			if seen[name] && !added[name] {
				kept = append(kept, name)
				added[name] = true
			}
		}
		return kept
	}
	def.Deferred, def.GlobalNames = keepNames(def.Deferred), keepNames(def.GlobalNames)
	return unique, def, cleanup
}

// guardPanic turns a panicking tool handler into an ordinary tool error. The
// harness runs each tool on its own goroutine, so a panic inside a handler can't
// be recovered by the caller that started the run — it takes the whole process
// down, and on restart the agent replays the same call and crashes again. Applied
// last, so it covers every tool the agent can reach: domain, SDK, MCP and skill.
func guardPanic(t actool.CoreTool) actool.CoreTool { return &guardedTool{CoreTool: t} }

type guardedTool struct{ actool.CoreTool }

// UnwrapTool exposes the original tool to trusted runtime placement/observation code.
func (g *guardedTool) UnwrapTool() actool.CoreTool { return g.CoreTool }

func (g *guardedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (res actool.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Name(), r, debug.Stack())
			res, err = actool.Errorf(fmt.Sprintf("工具 %s 内部错误：%v（本次调用已失败，可换个参数或改用别的工具）", g.Name(), r)), nil
		}
	}()
	return g.CoreTool.Call(ctx, in, tc)
}
