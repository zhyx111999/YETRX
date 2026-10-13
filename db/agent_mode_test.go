package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/minimalprompt"
	"github.com/google/uuid"
)

// These tests never fall back to config.json or the application's DSN. Each test
// creates and drops its own *_test database on an explicitly supplied local PG.
func nativeMigrationDB(t *testing.T, schema bool) (*DB, string) {
	t.Helper()
	dsn := os.Getenv("ARTEX_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("ARTEX_MIGRATION_TEST_DSN is unset; isolated PostgreSQL migration tests require a local *_test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") ||
		u.Port() == "" || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("ARTEX_MIGRATION_TEST_DSN must be a local postgres URL with an explicit port and a *_test database")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "native_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create isolated migration database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`); err != nil {
			t.Errorf("remove isolated migration database: %v", err)
		}
	})
	u.Path = "/" + name
	q := u.Query()
	q.Set("statement_timeout", "15000")
	u.RawQuery = q.Encode()
	raw, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(16)
	d := &DB{raw}
	t.Cleanup(func() { d.Close() })
	if schema {
		nativeExec(t, d, schemaSQL)
	}
	return d, u.String()
}

func nativeExec(t *testing.T, d *DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func nativeInt(t *testing.T, d *DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := d.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func nativeSnapshot(t *testing.T, d *DB, query string) string {
	t.Helper()
	var value string
	if err := d.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r)), '[]'::jsonb)::text FROM (` + query + `) r`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func nativeOpen(t *testing.T, dsn string) {
	t.Helper()
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

func nativeParallel(t *testing.T, n int, action func(int) error) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			errs <- action(i)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestNativeAgentFreshConcurrentOpen(t *testing.T) {
	d, dsn := nativeMigrationDB(t, false)
	nativeParallel(t, 6, func(int) error {
		opened, err := Open(dsn)
		if err == nil {
			opened.Close()
		}
		return err
	})
	a, err := d.GetAgentByKey("agent")
	if err != nil || a == nil {
		t.Fatalf("native agent: %v, %v", a, err)
	}
	if a.Name != "Agent" || a.Role != "assistant" || !a.Builtin || a.MaxTurns != 0 || a.RunSecs != 0 {
		t.Fatalf("native defaults: %+v", a)
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM agents WHERE builtin`); n != int64(len(builtinAgents)) {
		t.Fatalf("builtin count = %d", n)
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM agents WHERE key='auto'`); n != 0 {
		t.Fatal("auto was seeded")
	}
	prompt, err := d.CurrentPrompt(a.ID)
	if err != nil || prompt != minimalprompt.Default {
		t.Fatalf("native prompt: %q, %v", prompt, err)
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM agent_prompts WHERE agent_id=$1`, a.ID); n != 1 {
		t.Fatalf("concurrent initialization created %d prompt versions", n)
	}
	if n := nativeInt(t, d, `SELECT (SELECT count(*) FROM agent_skill_visibility WHERE agent_id=$1) + (SELECT count(*) FROM agent_visibility WHERE agent_id=$1)`, a.ID); n != 0 {
		t.Fatal("fresh Agent received resource bindings")
	}

	// Even a deliberate restore of the old default after migration must survive
	// restarts; the marker, not prompt content or the agent key, gates migration.
	nativeExec(t, d, `UPDATE agents SET name='My Agent',description='my settings',enabled=false,max_turns=13,run_seconds=1200,interactive_shell=true,web_search=true WHERE id=$1`, a.ID)
	if _, err := d.SavePrompt(a.ID, legacyPiPrompt, "deliberate restore", "system"); err != nil {
		t.Fatal(err)
	}
	nativeExec(t, d, `INSERT INTO tools(key,agents,enabled) VALUES ('kept','["auto","worker"]',false)`)
	nativeExec(t, d, `INSERT INTO agent_triggers(agent_key,enabled) VALUES ('auto',false)`)
	nativeExec(t, d, `INSERT INTO conversations(agent_key,title) VALUES ('auto','added after migration')`)
	queries := []string{
		`SELECT * FROM agents WHERE key='agent'`, `SELECT * FROM agent_prompts ORDER BY id`,
		`SELECT * FROM tools`, `SELECT * FROM agent_triggers`, `SELECT * FROM conversations`,
		`SELECT * FROM settings WHERE key='native_agent_v1'`,
	}
	before := make([]string, len(queries))
	for i, q := range queries {
		before[i] = nativeSnapshot(t, d, q)
	}
	for range 2 {
		nativeOpen(t, dsn)
	}
	for i, q := range queries {
		if got := nativeSnapshot(t, d, q); got != before[i] {
			t.Errorf("restart overwrote data: %s\nbefore %s\nafter  %s", q, before[i], got)
		}
	}
}

// Build a populated upstream / deployed-pi database without calling the new seed.
// The builtin ID is deliberately 5, and the active prompt is below a newer inactive
// version to exercise MAX(version), not current version + 1.
func nativeLegacyFixture(t *testing.T, d *DB, pi, edited bool) {
	t.Helper()
	key, name, description, prompt, seconds := "auto", "Auto", "平台操作助手：用工具管理任务(建/看/暂停/给提示)与资产，并可创建/修改 skill、自定义工具、MCP。", legacyAutoPrompt, 1200
	if pi {
		key, name, description, prompt, seconds = "agent", "Agent", "通用对话助手，使用 pi 原生工具与 workflow，按需读取 skills。", legacyPiPrompt, 0
	}
	nativeExec(t, d, `INSERT INTO llm_profiles(id,name,format,model,base_url,api_key,reasoning_effort,max_tokens,streaming,priority,is_default)
VALUES (1,'kept profile','openai','kept model','https://example.invalid','test-only-key','high',4096,false,8,true)`)
	nativeExec(t, d, `INSERT INTO agents(id,key,name,description,role,llm_profile_id,enabled,run_seconds,interactive_shell,web_search,wrapup_prompt)
VALUES (5,$1,$2,$3,'assistant',1,false,$4,$5,true,'keep wrapup')`, key, name, description, seconds, !pi)
	nativeExec(t, d, `SELECT setval('agents_id_seq',5)`)
	nativeExec(t, d, `INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
VALUES (5,1,$1,'original auto default','system'),(5,7,'inactive user edit','keep history','owner')`, legacyAutoPrompt)
	author := "system"
	if edited {
		prompt, author = "my prompt\n"+prompt, "owner"
		nativeExec(t, d, `UPDATE agents SET name='My Agent',description='custom description',max_turns=17,run_seconds=93 WHERE id=5`)
	}
	nativeExec(t, d, `INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) VALUES (5,3,$1,'active prompt',$2)`, prompt, author)
	nativeExec(t, d, `UPDATE agents SET current_prompt_id=(SELECT id FROM agent_prompts WHERE agent_id=5 AND version=3) WHERE id=5`)
	nativeExec(t, d, `INSERT INTO conversations(id,agent_key,title,llm_profile_id,pinned_at) VALUES (11,$1,'kept conversation',1,'2026-01-02'),(12,'review','other conversation',1,NULL)`, key)
	nativeExec(t, d, `INSERT INTO conversation_activities(conversation_id,worker,kind,summary,detail,tool_use_id) VALUES (11,'auto','text','history','keep full detail','kept-call')`)
	nativeExec(t, d, `INSERT INTO agent_triggers(agent_key,enabled,on_tool_call,tool_names,tool_call_message,interval_sec,last_fire)
VALUES ('auto',false,true,'kept_tool','user message',321,'2026-01-03'),('review',true,false,'','other',0,NULL)`)
	nativeExec(t, d, `INSERT INTO tools(key,system,kind,description,schema,agents,enabled,exec,deferred)
VALUES ('kept_tool',false,'script','edited description','{"type":"object"}','["worker","auto","review","agent","worker"]',false,'{"code":"kept"}',true),
('other_tool',true,'builtin','other','{}','["review"]',true,'{}',false)`)
	nativeExec(t, d, `INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,mcp_tool_name,enabled) VALUES (5,'mcp',123,'kept_mcp_tool',$1),(5,'mcp',124,'disabled',false)`, !pi)
	nativeExec(t, d, `INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES (5,'api-recon',$1),(5,'disabled-skill',false)`, !pi)
	nativeExec(t, d, `INSERT INTO agent_prompt_vars(agent_id,var_name,source) VALUES (5,'CustomVar','runtime'),(5,'Now','runtime')`)
	nativeExec(t, d, `INSERT INTO tool_usage(tool_key,agent_key,session_id) VALUES ('kept_tool','auto','conv-11')`)
	nativeExec(t, d, `INSERT INTO skill_usage(skill,agent_key,session_id) VALUES ('api-recon','auto','conv-11')`)
	nativeExec(t, d, `INSERT INTO settings(key,value) VALUES ('auto_default_bindings_v1','done'),('custom-setting','keep')`)
}

func TestNativeAgentUpgradePreservesData(t *testing.T) {
	for _, pi := range []bool{false, true} {
		for _, edited := range []bool{false, true} {
			t.Run(fmt.Sprintf("pi=%t/edited=%t", pi, edited), func(t *testing.T) {
				d, dsn := nativeMigrationDB(t, true)
				nativeLegacyFixture(t, d, pi, edited)
				queries := []string{
					`SELECT * FROM llm_profiles`, `SELECT * FROM conversation_activities`,
					`SELECT * FROM agent_prompts WHERE version<=7 ORDER BY id`,
					`SELECT * FROM agent_visibility ORDER BY resource_id`, `SELECT * FROM agent_skill_visibility WHERE agent_id=5 ORDER BY skill_name`,
					`SELECT * FROM agent_prompt_vars WHERE agent_id=5`, `SELECT * FROM tool_usage`, `SELECT * FROM skill_usage`,
					`SELECT to_jsonb(c)-'agent_key'-'updated_at' AS kept FROM conversations c ORDER BY id`,
					`SELECT to_jsonb(t)-'agent_key'-'updated_at' AS kept FROM agent_triggers t ORDER BY id`,
					`SELECT to_jsonb(t)-'agents'-'updated_at' AS kept FROM tools t ORDER BY key`,
					`SELECT * FROM settings WHERE key IN ('auto_default_bindings_v1','custom-setting') ORDER BY key`,
				}
				before := make([]string, len(queries))
				for i, q := range queries {
					before[i] = nativeSnapshot(t, d, q)
				}
				nativeOpen(t, dsn)
				for i, q := range queries {
					if got := nativeSnapshot(t, d, q); got != before[i] {
						t.Errorf("migration changed retained data: %s\nbefore %s\nafter  %s", q, before[i], got)
					}
				}
				a, err := d.GetAgentByKey("agent")
				if err != nil || a == nil {
					t.Fatalf("agent: %+v, %v", a, err)
				}
				if a.ID != 5 || a.Role != "assistant" || !a.Builtin || a.Enabled || !a.WebSearch || a.InteractiveShell == pi || a.LLMProfileID == nil || *a.LLMProfileID != 1 || a.WrapupPrompt != "keep wrapup" {
					t.Fatalf("retained agent configuration: %+v", a)
				}
				wantVersion, wantCount := int64(8), int64(4)
				if edited {
					wantVersion, wantCount = 3, 3
					if a.Name != "My Agent" || a.Description != "custom description" || a.MaxTurns != 17 || a.RunSecs != 93 {
						t.Fatalf("edited metadata changed: %+v", a)
					}
				} else {
					if a.Name != "Agent" || a.Description != minimalprompt.AgentDescription || a.MaxTurns != 0 || a.RunSecs != 0 {
						t.Fatalf("default metadata: %+v", a)
					}
					if prompt, err := d.CurrentPrompt(5); err != nil || prompt != minimalprompt.Default {
						t.Fatalf("native prompt: %q, %v", prompt, err)
					}
				}
				if n := nativeInt(t, d, `SELECT version FROM agent_prompts WHERE id=(SELECT current_prompt_id FROM agents WHERE id=5)`); n != wantVersion {
					t.Fatalf("current version=%d want %d", n, wantVersion)
				}
				if n := nativeInt(t, d, `SELECT count(*) FROM tools WHERE key='kept_tool' AND agents='["worker","agent","review","agent","worker"]'::jsonb`); n != 1 {
					t.Fatal("tool binding order or other entries changed")
				}
				if n := nativeInt(t, d, `SELECT (SELECT count(*) FROM conversations WHERE id=11 AND agent_key='agent') + (SELECT count(*) FROM agent_triggers WHERE agent_key='agent')`); n != 2 {
					t.Fatal("conversation / trigger references not migrated")
				}
				nativeOpen(t, dsn)
				if n := nativeInt(t, d, `SELECT count(*) FROM agent_prompts WHERE agent_id=5`); n != wantCount {
					t.Fatalf("prompt versions after restart=%d want %d", n, wantCount)
				}
			})
		}
	}
}
