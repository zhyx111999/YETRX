package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// 回归：所有 ChatAgent 模式（内置 Agent、自定义模式）
// 都经同一条全局装配链拿资源——
//
//	ChatAgent.Chat → AugmentTools(base=SDK DefaultTools)
//	  → ToolAugment(wireAgentAugment): 全局 skill 目录 + agent_skill_visibility、
//	    mcp_servers + agent_visibility、hostTools(DB 自定义函数工具)
//	  → ToolResolve(wireTools): tools 表的 描述/schema/启用/按 agent 绑定
//
// 这里用一个内置模式(agent)和一个新建的自定义模式做对照：同一份全局资源改一次，
// 两个模式的下一次装配都必须跟着变；绑定/可见性只影响被绑定的那个模式。
// 需要 PostgreSQL（ARTEX_PG_DSN / config.json），不可用时跳过。

// nativeEnv 是两个对照模式 + 一套各自带唯一后缀的全局资源。
type nativeEnv struct {
	t        *testing.T
	pg       *db.DB
	skillDir string
	keys     []string // 对照的模式 key：[内置 Agent, 自定义]
	ids      map[string]int64

	skill   string // skill 目录名（= 可见性键）
	mcp     string // MCP server 名
	mcpID   int64
	mcpTool string // mcp__<server>__ping
	custom  string // DB 自定义函数工具 key
}

const nativeSystemTool = "list_companies" // tools 表里的内置域工具，Agent 默认已绑定

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fakeMCPHTTP 实现最小 Streamable-HTTP MCP：initialize / tools/list(ping) / tools/call。
func fakeMCPHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.ID) == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "ping", "description": "nr ping", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeNativeSkill(t *testing.T, dir, name, desc, mcps string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	front := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if mcps != "" {
		front += "mcps: " + mcps + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(front+"---\nbody of "+name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newNativeEnv(t *testing.T) *nativeEnv {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	sfx := randSuffix()
	e := &nativeEnv{
		t: t, pg: pg, skillDir: t.TempDir(), ids: map[string]int64{},
		skill: "nr-skill-" + sfx, mcp: "nrmcp" + sfx, custom: "nr_tool_" + sfx,
	}
	e.mcpTool = "mcp__" + e.mcp + "__ping"

	// 模式 1：内置 Agent；模式 2：新建自定义模式（key 前缀 nr_mode_，测试后删除）。
	custom := "nr_mode_" + sfx
	if _, err := pg.CreateAgent(custom, "NR 对照模式", ""); err != nil {
		t.Fatalf("create custom mode: %v", err)
	}
	t.Cleanup(func() {
		_ = pg.RemoveAgentFromToolBindings(custom)
		_ = pg.DeleteAgent(custom)
	})
	e.keys = []string{"agent", custom}
	for _, k := range e.keys {
		a, err := pg.GetAgentByKey(k)
		if err != nil || a == nil {
			t.Fatalf("agent %q missing: %v", k, err)
		}
		e.ids[k] = a.ID
	}

	// 装配钩子与生产一致（server.New 里同样三步）；测试后还原。
	s := &Server{m: &Manager{pg: pg}, skillDir: e.skillDir}
	prevAugment, prevResolve, prevTraffic := agent.ToolAugment, agent.ToolResolve, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve, agent.FindingTrafficBindingEnabled = prevAugment, prevResolve, prevTraffic
	})
	wireAgentAugment(pg, e.skillDir, s.hostTools)
	wireTools(pg, buildDomainReg(pg.Assets()))

	// 全局资源各建一份，初始对两个模式都可见/已绑定。
	writeNativeSkill(t, e.skillDir, e.skill, "skill v1", "")
	for _, k := range e.keys {
		if err := pg.ToggleSkillVisibility(e.ids[k], e.skill, true); err != nil {
			t.Fatal(err)
		}
		k := k
		t.Cleanup(func() { _ = pg.ToggleSkillVisibility(e.ids[k], e.skill, false) })
	}

	mcpSrv := fakeMCPHTTP(t)
	e.mcpID, err = pg.SaveMCP(&db.MCPServer{Name: e.mcp, Transport: "http", URL: mcpSrv.URL, Enabled: true})
	if err != nil {
		t.Fatalf("save mcp: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteMCP(e.mcpID) })
	for _, k := range e.keys {
		e.setMCPVisible(k, true)
	}

	if err := pg.CreateCustomTool(&db.Tool{
		Key: e.custom, Description: "custom v1", Agents: e.keys, Enabled: true, Kind: "command",
		Schema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
		Exec:   json.RawMessage(`{"command":"echo nr-{msg}"}`),
	}); err != nil {
		t.Fatalf("create custom tool: %v", err)
	}
	t.Cleanup(func() {
		_ = pg.DeleteCustomTool(e.custom)
		_, _ = pg.Exec(`DELETE FROM tool_usage WHERE tool_key=$1`, e.custom) // Chat 里真调用过 → 账本也清掉
	})

	// 内置域工具：记下原行，测试后原样还原；初始绑定到两个模式。
	orig, err := pg.GetTool(nativeSystemTool)
	if err != nil || orig == nil {
		t.Fatalf("%s not seeded: %v", nativeSystemTool, err)
	}
	t.Cleanup(func() {
		_ = pg.UpdateTool(orig.Key, orig.Description, orig.Schema, mustJSON(orig.Agents), orig.Enabled)
	})
	e.setSystemTool(orig.Description, e.keys, true)
	return e
}

func (e *nativeEnv) setMCPVisible(key string, on bool) {
	e.t.Helper()
	var err error
	if on {
		_, err = e.pg.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled) VALUES ($1,'mcp',$2,true)
ON CONFLICT (agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=true`, e.ids[key], e.mcpID)
	} else {
		_, err = e.pg.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=$2`, e.ids[key], e.mcpID)
	}
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *nativeEnv) setSystemTool(desc string, agents []string, enabled bool) {
	e.t.Helper()
	cur, err := e.pg.GetTool(nativeSystemTool)
	if err != nil || cur == nil {
		e.t.Fatalf("get %s: %v", nativeSystemTool, err)
	}
	if err := e.pg.UpdateTool(cur.Key, desc, cur.Schema, mustJSON(agents), enabled); err != nil {
		e.t.Fatal(err)
	}
}

func (e *nativeEnv) setCustomTool(desc string, agents []string, enabled bool) {
	e.t.Helper()
	cur, err := e.pg.GetTool(e.custom)
	if err != nil || cur == nil {
		e.t.Fatalf("get %s: %v", e.custom, err)
	}
	cur.Description, cur.Agents, cur.Enabled = desc, agents, enabled
	if err := e.pg.UpdateCustomTool(cur); err != nil {
		e.t.Fatal(err)
	}
}

func (e *nativeEnv) setMCPEnabled(on bool) {
	e.t.Helper()
	if _, err := e.pg.Exec(`UPDATE mcp_servers SET enabled=$2 WHERE id=$1`, e.mcpID, on); err != nil {
		e.t.Fatal(err)
	}
}

// nativeView 是某个模式一次装配后看到的全局资源。
type nativeView struct {
	tools    map[string]actool.CoreTool
	def      agent.DeferredInfo
	done     func()
	skills   string // Skill 元工具的 Prompt（可用 skill 清单）
	mcpReady bool   // MCP 工具已进全局（开局可调）
	mcpGated bool   // MCP 工具被装配但锁在 skill 之后
}

// view 与 ChatAgent.Chat 一模一样：以 SDK DefaultTools 为 base 调 AugmentTools。
func (e *nativeEnv) view(key string) *nativeView {
	e.t.Helper()
	tools, def, cleanup := agent.AugmentTools(context.Background(), key, actool.DefaultTools())
	v := &nativeView{tools: names(tools), def: def, done: cleanup}
	if sk := v.tools["Skill"]; sk != nil {
		v.skills = sk.Prompt()
	}
	global := false
	for _, n := range def.GlobalNames {
		global = global || n == e.mcpTool
	}
	deferred := false
	for _, n := range def.Deferred {
		deferred = deferred || n == e.mcpTool
	}
	v.mcpReady = global && v.tools[e.mcpTool] != nil
	v.mcpGated = !global && deferred && v.tools[e.mcpTool] != nil
	return v
}

type resources struct {
	sdk      bool   // base SDK 工具原样在（Bash/Read）
	skill    bool   // 该 skill 出现在 Skill 元工具清单
	skillTxt string // 清单里该 skill 的描述（取 skill v?）
	mcp      bool   // MCP 工具开局可调
	custom   string // DB 自定义函数工具当前描述；"" = 不存在
	system   string // tools 表内置域工具当前描述；"" = 不存在
}

func (e *nativeEnv) read(key string) resources {
	v := e.view(key)
	defer v.done()
	r := resources{
		sdk:   v.tools["Bash"] != nil && v.tools["Read"] != nil,
		skill: strings.Contains(v.skills, e.skill),
		mcp:   v.mcpReady,
	}
	for _, ver := range []string{"skill v1", "skill v2"} {
		if strings.Contains(v.skills, ver) {
			r.skillTxt = ver
		}
	}
	if t := v.tools[e.custom]; t != nil {
		r.custom = t.Description()
	}
	if t := v.tools[nativeSystemTool]; t != nil {
		r.system = t.Description()
	}
	return r
}

// both 断言两个模式读到的资源相同，并返回该值。
func (e *nativeEnv) both(t *testing.T, label string) resources {
	t.Helper()
	a, b := e.read(e.keys[0]), e.read(e.keys[1])
	if a != b {
		t.Errorf("%s: 两个模式看到的全局资源不一致\n  %s: %+v\n  %s: %+v", label, e.keys[0], a, e.keys[1], b)
	}
	return a
}

func TestNativeModesShareGlobalResources(t *testing.T) {
	e := newNativeEnv(t)
	all := e.keys

	t.Run("baseline", func(t *testing.T) {
		got := e.both(t, "baseline")
		want := resources{sdk: true, skill: true, skillTxt: "skill v1", mcp: true, custom: "custom v1", system: got.system}
		if got != want || got.system == "" {
			t.Fatalf("baseline = %+v, want SDK+skill+MCP+custom+system tool all present (%+v)", got, want)
		}
	})

	t.Run("description_and_skill_edits_reach_every_mode", func(t *testing.T) {
		e.setCustomTool("custom v2", all, true)
		e.setSystemTool("system v2", all, true)
		writeNativeSkill(t, e.skillDir, e.skill, "skill v2", "") // 只改全局 skill 目录里的文件
		defer func() {
			e.setCustomTool("custom v1", all, true)
			writeNativeSkill(t, e.skillDir, e.skill, "skill v1", "")
		}()
		got := e.both(t, "edits")
		if got.custom != "custom v2" || got.system != "system v2" || got.skillTxt != "skill v2" {
			t.Fatalf("edits not visible to both modes: %+v", got)
		}
	})

	t.Run("switches_apply_to_every_mode", func(t *testing.T) {
		e.setCustomTool("custom v1", all, false)
		if got := e.both(t, "custom off"); got.custom != "" {
			t.Errorf("custom tool disabled but still present: %+v", got)
		}
		e.setCustomTool("custom v1", all, true)

		e.setSystemTool("system off", all, false)
		if got := e.both(t, "system off"); got.system != "" {
			t.Errorf("system tool disabled but still present: %+v", got)
		}
		e.setSystemTool("system on", all, true)

		e.setMCPEnabled(false)
		if got := e.both(t, "mcp off"); got.mcp {
			t.Errorf("MCP disabled but still present: %+v", got)
		}
		e.setMCPEnabled(true)

		// 删掉全局目录里的 skill 文件 → 两个模式的 Skill 清单同时消失。
		if err := os.RemoveAll(filepath.Join(e.skillDir, e.skill)); err != nil {
			t.Fatal(err)
		}
		if got := e.both(t, "skill removed"); got.skill {
			t.Errorf("skill removed from global dir but still listed: %+v", got)
		}
		writeNativeSkill(t, e.skillDir, e.skill, "skill v1", "")

		if got := e.both(t, "restored"); !got.mcp || got.custom == "" || got.system == "" || !got.skill || !got.sdk {
			t.Fatalf("restore failed: %+v", got)
		}
	})

	// 绑定/可见性只影响被配置的那个模式；两个方向都验，避免偏向某一个 key。
	t.Run("bindings_are_per_mode", func(t *testing.T) {
		for i, only := range all {
			other := all[1-i]
			e.setCustomTool("custom v1", []string{only}, true)
			e.setSystemTool("system v1", []string{only}, true)
			for _, k := range all {
				e.setMCPVisible(k, k == only)
				if err := e.pg.ToggleSkillVisibility(e.ids[k], e.skill, k == only); err != nil {
					t.Fatal(err)
				}
			}
			got, miss := e.read(only), e.read(other)
			if got.custom == "" || got.system == "" || !got.mcp || !got.skill {
				t.Errorf("only=%s: bound mode lost a resource: %+v", only, got)
			}
			if miss.custom != "" || miss.system != "" || miss.mcp || miss.skill {
				t.Errorf("only=%s: unbound mode %s still sees resources: %+v", only, other, miss)
			}
			if !got.sdk || !miss.sdk { // SDK 基础工具不受绑定影响
				t.Errorf("only=%s: SDK base tools must stay for every mode (%v/%v)", only, got.sdk, miss.sdk)
			}
		}
		e.setCustomTool("custom v1", all, true)
		e.setSystemTool("system v1", all, true)
		for _, k := range all {
			e.setMCPVisible(k, true)
			if err := e.pg.ToggleSkillVisibility(e.ids[k], e.skill, true); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.both(t, "rebound"); got.custom == "" || got.system == "" || !got.mcp || !got.skill {
			t.Fatalf("rebind failed: %+v", got)
		}
	})

	// skill 的 `mcps:` 声明让 MCP 被「锁在 skill 后」：装配但不进开局全局清单，
	// 且对两个模式一视同仁（MCP 未直接可见，仅靠 skill 引用）。
	t.Run("skill_gated_mcp_same_for_every_mode", func(t *testing.T) {
		gated := "nr-gated-" + randSuffix()
		writeNativeSkill(t, e.skillDir, gated, "gates the mcp", e.mcp)
		for _, k := range all {
			e.setMCPVisible(k, false)
			if err := e.pg.ToggleSkillVisibility(e.ids[k], gated, true); err != nil {
				t.Fatal(err)
			}
			k := k
			defer func() { _ = e.pg.ToggleSkillVisibility(e.ids[k], gated, false) }()
		}
		defer func() {
			for _, k := range all {
				e.setMCPVisible(k, true)
			}
		}()
		for _, k := range all {
			v := e.view(k)
			if !v.mcpGated {
				t.Errorf("%s: MCP should be deferred+locked behind the gating skill (ready=%v gated=%v)", k, v.mcpReady, v.mcpGated)
			}
			// 调用 Skill 后解锁：UnlockSkill 与 history 回放共用同一入口。
			v.def.UnlockSkill(gated)
			if !v.def.Unlock.Has(e.mcpTool) {
				t.Errorf("%s: loading the gating skill did not unlock the MCP tool", k)
			}
			v.done()
		}
	})

	t.Run("tools_are_callable_from_every_mode", func(t *testing.T) {
		for _, k := range all {
			v := e.view(k)
			res, err := v.tools[e.custom].Call(context.Background(), json.RawMessage(`{"msg":"hi"}`), &actool.ToolContext{WorkingDir: t.TempDir()})
			if err != nil || res.IsError || !strings.Contains(res.Flatten(), "nr-hi") {
				t.Errorf("%s: custom tool call = %q err=%v isErr=%v", k, res.Flatten(), err, res.IsError)
			}
			res, err = v.tools[e.mcpTool].Call(context.Background(), json.RawMessage(`{}`), nil)
			if err != nil || res.IsError || res.Flatten() != "pong" {
				t.Errorf("%s: MCP tool call = %q err=%v isErr=%v", k, res.Flatten(), err, res.IsError)
			}
			v.done()
		}
	})
}

// recordingProvider 记录每次请求，并按脚本回放：第 1 轮调自定义函数工具，之后给文本。
type recordingProvider struct {
	mu       sync.Mutex
	reqs     []llm.CompletionRequest
	callTool string
}

func (p *recordingProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if len(p.reqs) == 1 && p.callTool != "" {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "nr-call", Name: p.callTool, Input: json.RawMessage(`{"msg":"e2e"}`)},
		}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
}

func (p *recordingProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		msg, stop, _, err := p.Complete(ctx, req)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				if !y(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) ||
					!y(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			} else if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
				return
			}
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

// TestChatAgentRequestCarriesGlobalResources 走真实 ChatAgent.Chat（只替换 LLM provider）：
// 两个模式发给模型的请求里都带着同一份全局 skill 清单、DB 自定义函数工具(描述为 DB 值)、
// 内置域工具与 SDK 基础工具；MCP 工具名进开局 deferred 清单；模型调用自定义函数工具能真正执行。
func TestChatAgentRequestCarriesGlobalResources(t *testing.T) {
	e := newNativeEnv(t)
	for _, key := range e.keys {
		t.Run(key, func(t *testing.T) {
			p := &recordingProvider{callTool: e.custom}
			chat := agent.NewChatAgent(p, "fixture", t.TempDir(), nil, 100000)
			chat.SetNonStreaming(func() bool { return true })

			var results []db.Activity
			if _, err := chat.Chat(context.Background(), key, "nr-"+randSuffix(), "hello", 5, 0, false, func(a db.Activity) {
				if a.Kind == "tool_result" {
					results = append(results, a)
				}
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if len(p.reqs) < 2 {
				t.Fatalf("expected a tool round-trip (2 model calls), got %d", len(p.reqs))
			}
			req := p.reqs[0]
			byName := map[string]llm.ToolSchema{}
			for _, ts := range req.Tools {
				byName[ts.Name] = ts
			}
			for _, name := range []string{"Bash", "Read", "Skill", e.custom, nativeSystemTool} {
				if _, ok := byName[name]; !ok {
					t.Errorf("request tools lack %q", name)
				}
			}
			if d := byName[e.custom].Description; !strings.Contains(d, "custom v1") {
				t.Errorf("custom tool description in request = %q, want the DB value", d)
			}
			if d := byName["Skill"].Description; !strings.Contains(d, e.skill) {
				t.Errorf("Skill tool in request does not list the global skill %q", e.skill)
			}
			if sys := strings.Join(req.System, "\n"); !strings.Contains(sys, e.mcpTool) {
				t.Errorf("system prompt lacks the deferred MCP tool %q", e.mcpTool)
			}
			ok := false
			for _, r := range results {
				ok = ok || (!r.IsError && r.Tool == e.custom && strings.Contains(r.Detail+r.Summary, "nr-e2e"))
			}
			if !ok {
				t.Errorf("custom function tool did not execute through Chat; tool_result activities: %+v", results)
			}
		})
	}
}
