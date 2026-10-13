package db

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/minimalprompt"
	"github.com/google/uuid"
)

// These integration tests never use DSN()/config.json. Point this explicit URL
// at a dedicated database whose name ends in _test. Each case creates and drops
// its own schema. Example:
// ARTEX_AGENT_MIGRATION_TEST_DSN=postgres://user:pass@localhost/yetrx_agent_test?sslmode=disable
func agentModeTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dsn := os.Getenv("ARTEX_AGENT_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("ARTEX_AGENT_MIGRATION_TEST_DSN is unset; requires a separate PostgreSQL test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("ARTEX_AGENT_MIGRATION_TEST_DSN must be a PostgreSQL URL with a database name ending in _test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("test PostgreSQL unavailable: %v", err)
	}
	schema := "agent_mode_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	query.Set("connect_timeout", "5")
	u.RawQuery = query.Encode()
	dsn = u.String()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	d := &DB{raw}
	t.Cleanup(func() { d.Close() })
	// Prepare the actual schema without running current seeds, so fixtures can
	// describe a pre-upgrade database. Open below exercises the real startup.
	if err := applySchemaWithRetry(t.Context(), raw, time.Sleep); err != nil {
		t.Fatal(err)
	}
	return d, dsn
}

func agentModeOpen(t *testing.T, dsn string) *DB {
	t.Helper()
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func migrationExec(t *testing.T, d *DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func migrationID(t *testing.T, d *DB, query string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(query, args...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func migrationJSON(t *testing.T, d *DB, query string) string {
	t.Helper()
	var got string
	if err := d.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func assertAgentModeDefaults(t *testing.T, d *DB, wantID int64) *Agent {
	t.Helper()
	a, err := d.GetAgentByKey("agent")
	if err != nil || a == nil {
		t.Fatalf("get Agent: %v / %v", a, err)
	}
	if wantID != 0 && a.ID != wantID {
		t.Fatalf("Agent id=%d, want preserved id=%d", a.ID, wantID)
	}
	if a.Name != "Agent" || a.Role != "assistant" || !a.Builtin || !a.Enabled ||
		a.MaxTurns != 0 || a.RunSecs != 0 || a.WebSearch || a.InteractiveShell {
		t.Fatalf("unexpected Agent defaults: %+v", a)
	}
	prompt, err := d.CurrentPrompt(a.ID)
	if err != nil || prompt != minimalprompt.Default {
		t.Fatalf("current prompt is not the minimal Agent prompt: %q / %v", prompt, err)
	}
	return a
}

func TestAgentModeMigrationPreservesHistory(t *testing.T) {
	d, dsn := agentModeTestDB(t)
	profileID := migrationID(t, d, `INSERT INTO llm_profiles(name,format,model) VALUES ('bound model','openai','test-model') RETURNING id`)
	autoID := migrationID(t, d, `INSERT INTO agents(key,name,role,builtin,enabled,llm_profile_id,max_turns,run_seconds,web_search,interactive_shell)
VALUES ('auto','Custom Auto','assistant',true,false,$1,42,987,true,true) RETURNING id`, profileID)
	if _, err := d.SavePrompt(autoID, "original Auto default", "original note", "system"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SavePrompt(autoID, "user-authored Auto prompt", "keep this edit", "owner"); err != nil {
		t.Fatal(err)
	}
	// The active version need not be the highest version.
	migrationExec(t, d, `INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) VALUES ($1,7,'older inactive edit','keep inactive','owner')`, autoID)
	convID := migrationID(t, d, `INSERT INTO conversations(agent_key,title,llm_profile_id,pinned_at) VALUES ('auto','Pinned history',$1,now()) RETURNING id`, profileID)
	migrationExec(t, d, `INSERT INTO conversations(agent_key,title) VALUES ('pentest','Other mode')`)
	for _, kind := range []string{"user", "assistant", "tool_use", "tool_result", "error", "workflow"} {
		migrationExec(t, d, `INSERT INTO conversation_activities(conversation_id,worker,kind,tool,tool_use_id,summary,detail,input_tokens,output_tokens)
VALUES ($1,'auto',$2,'legacy_tool','call-1','saved summary','saved details',11,22)`, convID, kind)
	}
	migrationExec(t, d, `INSERT INTO tools(key,system,kind,agents,description) VALUES ('custom_history',false,'script','["auto","worker"]','user description')`)
	migrationExec(t, d, `INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,mcp_tool_name) VALUES ($1,'mcp',123,'custom_mcp_tool')`, autoID)
	migrationExec(t, d, `INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES ($1,'api-recon',true),($1,'custom-skill',true),($1,'disabled-skill',false)`, autoID)
	migrationExec(t, d, `INSERT INTO agent_prompt_vars(agent_id,var_name,source) VALUES ($1,'CustomVar','runtime')`, autoID)
	migrationExec(t, d, `INSERT INTO agent_triggers(agent_key,enabled,on_tool_call,tool_names,tool_call_message,interval_sec,last_fire)
VALUES ('auto',true,true,'["legacy_tool"]','custom trigger',60,now()),('auto',false,false,'','disabled trigger',0,NULL)`)
	migrationExec(t, d, `INSERT INTO tool_usage(tool_key,agent_key,session_id) VALUES ('legacy_tool','auto','conv-history')`)
	migrationExec(t, d, `INSERT INTO skill_usage(skill,agent_key,session_id) VALUES ('custom-skill','auto','conv-history')`)

	unchanged := []string{
		`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM agent_prompts p WHERE version<=7`,
		`SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM conversation_activities a`,
		`SELECT jsonb_agg(to_jsonb(c)-'agent_key'-'updated_at' ORDER BY id)::text FROM conversations c`,
		`SELECT jsonb_agg(to_jsonb(t) ORDER BY key)::text FROM tools t`,
		`SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM agent_triggers t`,
		`SELECT jsonb_agg(to_jsonb(v)-'enabled' ORDER BY resource_id)::text FROM agent_visibility v`,
		`SELECT jsonb_agg(to_jsonb(v)-'enabled' ORDER BY agent_id,skill_name)::text FROM agent_skill_visibility v WHERE agent_id=(SELECT id FROM agents WHERE key IN ('auto','agent'))`,
		`SELECT jsonb_agg(to_jsonb(v) ORDER BY id)::text FROM agent_prompt_vars v WHERE var_name='CustomVar'`,
		`SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM tool_usage u`,
		`SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM skill_usage u`,
	}
	before := make([]string, len(unchanged))
	for i, query := range unchanged {
		before[i] = migrationJSON(t, d, query)
	}
	d = agentModeOpen(t, dsn)
	a := assertAgentModeDefaults(t, d, autoID)
	if a.LLMProfileID == nil || *a.LLMProfileID != profileID {
		t.Fatalf("lost LLM binding: %+v", a)
	}
	if old, err := d.GetAgentByKey("auto"); err != nil || old != nil {
		t.Fatalf("builtin Auto still exists: %v / %v", old, err)
	}
	for i, query := range unchanged {
		if got := migrationJSON(t, d, query); got != before[i] {
			t.Errorf("history changed for query %s\nbefore=%s\nafter=%s", query, before[i], got)
		}
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM conversations WHERE id=$1 AND agent_key='agent'`, convID); got != 1 {
		t.Fatal("Auto conversation was not migrated in place")
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM conversations WHERE agent_key='pentest'`); got != 1 {
		t.Fatal("another mode's conversation changed")
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM agent_prompts WHERE agent_id=$1`, autoID); got != 4 {
		t.Fatalf("prompt versions=%d, want three preserved + one new", got)
	}
	if got := migrationID(t, d, `SELECT p.version FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, autoID); got != 8 {
		t.Fatalf("current version=%d, want max historical version + 1", got)
	}
	if got := migrationID(t, d, `SELECT (SELECT COUNT(*) FROM agent_visibility WHERE agent_id=$1 AND enabled)+(SELECT COUNT(*) FROM agent_skill_visibility WHERE agent_id=$1 AND enabled)`, autoID); got != 0 {
		t.Fatal("Agent inherited legacy MCP/skill visibility")
	}

	// Subsequent user edits, including deliberate visibility changes, must survive
	// a full startup without another default prompt version or metadata refresh.
	migrationExec(t, d, `UPDATE agents SET name='My Agent',description='my settings',enabled=false,max_turns=5,run_seconds=73,web_search=true,interactive_shell=true WHERE id=$1`, autoID)
	migrationExec(t, d, `UPDATE agent_skill_visibility SET enabled=true WHERE agent_id=$1 AND skill_name='custom-skill'`, autoID)
	migrationExec(t, d, `UPDATE agent_visibility SET enabled=true WHERE agent_id=$1`, autoID)
	if _, err := d.SavePrompt(autoID, "user's new Agent prompt", "after migration", "owner"); err != nil {
		t.Fatal(err)
	}
	restartQueries := []string{
		`SELECT to_jsonb(a)::text FROM agents a WHERE key='agent'`,
		`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM agent_prompts p`,
		`SELECT jsonb_agg(to_jsonb(v) ORDER BY resource_id)::text FROM agent_visibility v`,
		`SELECT jsonb_agg(to_jsonb(v) ORDER BY agent_id,skill_name)::text FROM agent_skill_visibility v`,
	}
	restartBefore := make([]string, len(restartQueries))
	for i, query := range restartQueries {
		restartBefore[i] = migrationJSON(t, d, query)
	}
	d = agentModeOpen(t, dsn)
	for i, query := range restartQueries {
		if got := migrationJSON(t, d, query); got != restartBefore[i] {
			t.Errorf("restart overwrote user data: %s", query)
		}
	}
}

func TestAgentModeFreshSeedConcurrentStartup(t *testing.T) {
	d, dsn := agentModeTestDB(t)
	results := make(chan error, 3)
	for range 3 {
		go func() {
			opened, err := Open(dsn)
			if opened != nil {
				opened.Close()
			}
			results <- err
		}()
	}
	for range 3 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	a := assertAgentModeDefaults(t, d, 0)
	if got := migrationID(t, d, `SELECT COUNT(*) FROM agent_prompts WHERE agent_id=$1`, a.ID); got != 1 {
		t.Fatalf("concurrent startup created %d prompt versions, want 1", got)
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM agents WHERE key='auto'`); got != 0 {
		t.Fatal("fresh seed recreated Auto")
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM agent_skill_visibility WHERE agent_id=$1`, a.ID); got != 0 {
		t.Fatal("fresh Agent received default skill bindings")
	}
	pentest, err := d.GetAgentByKey("pentest")
	if err != nil || pentest == nil || !pentest.InteractiveShell || pentest.RunSecs != 0 {
		t.Fatalf("existing pentest defaults changed: %v / %v", pentest, err)
	}
	worker, err := d.GetAgentByKey("worker")
	if err != nil || worker == nil || !worker.InteractiveShell || worker.RunSecs != 1200 {
		t.Fatalf("existing worker defaults changed: %v / %v", worker, err)
	}
}

func TestAgentModeConflictPreservesRows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		legacy        bool
		targetBuiltin bool
	}{
		{"custom agent with Auto", true, false},
		{"custom agent without Auto", false, false},
		{"both builtin keys", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, dsn := agentModeTestDB(t)
			migrationExec(t, d, `INSERT INTO agents(key,name,role,builtin) VALUES ('agent','Keep target','assistant',$1)`, tc.targetBuiltin)
			if tc.legacy {
				migrationExec(t, d, `INSERT INTO agents(key,name,role,builtin) VALUES ('auto','Keep Auto','assistant',true)`)
			}
			migrationExec(t, d, `INSERT INTO agent_prompts(agent_id,version,template_text) SELECT id,1,'keep prompt' FROM agents`)
			migrationExec(t, d, `UPDATE agents SET current_prompt_id=(SELECT id FROM agent_prompts WHERE agent_id=agents.id)`)
			migrationExec(t, d, `INSERT INTO conversations(agent_key,title) VALUES ('auto','keep conversation')`)
			queries := []string{
				`SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM agents a`,
				`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM agent_prompts p`,
				`SELECT jsonb_agg(to_jsonb(c) ORDER BY id)::text FROM conversations c`,
			}
			before := make([]string, len(queries))
			for i, query := range queries {
				before[i] = migrationJSON(t, d, query)
			}
			opened, err := Open(dsn)
			if opened != nil {
				opened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "agent key conflict") {
				t.Fatalf("want explicit conflict, got %v", err)
			}
			for i, query := range queries {
				if got := migrationJSON(t, d, query); got != before[i] {
					t.Errorf("conflict changed data: %s", query)
				}
			}
		})
	}
}

func TestAgentModeRollbackAndRetry(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			d, dsn := agentModeTestDB(t)
			if legacy {
				id := migrationID(t, d, `INSERT INTO agents(key,name,role,interactive_shell) VALUES ('auto','Auto','assistant',true) RETURNING id`)
				if _, err := d.SavePrompt(id, "custom legacy prompt", "keep", "owner"); err != nil {
					t.Fatal(err)
				}
				migrationExec(t, d, `INSERT INTO agent_skill_visibility(agent_id,skill_name) VALUES ($1,'api-recon')`, id)
				migrationExec(t, d, `INSERT INTO agent_visibility(agent_id,resource_kind,resource_id) VALUES ($1,'mcp',123)`, id)
			}
			migrationExec(t, d, `INSERT INTO conversations(agent_key,title) VALUES ('auto','must survive failure')`)
			// Fail at the last data write, after renaming/creating Agent, disabling
			// bindings and inserting its prompt. The whole migration must roll back.
			migrationExec(t, d, `CREATE FUNCTION reject_agent_mode() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected Agent migration failure'; END $$;
CREATE TRIGGER reject_agent_mode BEFORE UPDATE ON conversations FOR EACH ROW EXECUTE FUNCTION reject_agent_mode()`)
			queries := []string{
				`SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]')::text FROM agents a`,
				`SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY id),'[]')::text FROM agent_prompts p`,
				`SELECT COALESCE(jsonb_agg(to_jsonb(v) ORDER BY skill_name),'[]')::text FROM agent_skill_visibility v`,
				`SELECT COALESCE(jsonb_agg(to_jsonb(v) ORDER BY resource_id),'[]')::text FROM agent_visibility v`,
				`SELECT jsonb_agg(to_jsonb(c) ORDER BY id)::text FROM conversations c`,
			}
			before := make([]string, len(queries))
			for i, query := range queries {
				before[i] = migrationJSON(t, d, query)
			}
			opened, err := Open(dsn)
			if opened != nil {
				opened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "injected Agent migration failure") {
				t.Fatalf("want injected failure, got %v", err)
			}
			for i, query := range queries {
				if got := migrationJSON(t, d, query); got != before[i] {
					t.Errorf("rollback failed for: %s", query)
				}
			}
			migrationExec(t, d, `DROP TRIGGER reject_agent_mode ON conversations`)
			d = agentModeOpen(t, dsn)
			assertAgentModeDefaults(t, d, 0)
		})
	}
}

func TestAgentModeExistingBuiltinIsNotReseeded(t *testing.T) {
	d, dsn := agentModeTestDB(t)
	id := migrationID(t, d, `INSERT INTO agents(key,name,description,role,builtin,enabled,max_turns,run_seconds)
VALUES ('agent','My Agent','user description','assistant',true,false,13,99) RETURNING id`)
	if _, err := d.SavePrompt(id, "already migrated and edited", "user version", "owner"); err != nil {
		t.Fatal(err)
	}
	query := `SELECT to_jsonb(a)::text FROM agents a WHERE key='agent'`
	before := migrationJSON(t, d, query)
	d = agentModeOpen(t, dsn)
	if got := migrationJSON(t, d, query); got != before {
		t.Fatalf("existing Agent was reseeded: before=%s after=%s", before, got)
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM agent_prompts WHERE agent_id=$1`, id); got != 1 {
		t.Fatalf("existing Agent received another prompt version: count=%d", got)
	}
}

func TestAgentModeCustomAutoIsPreserved(t *testing.T) {
	d, dsn := agentModeTestDB(t)
	id := migrationID(t, d, `INSERT INTO agents(key,name,role,builtin) VALUES ('auto','User Auto','assistant',false) RETURNING id`)
	migrationExec(t, d, `INSERT INTO conversations(agent_key,title) VALUES ('auto','custom Auto conversation')`)
	d = agentModeOpen(t, dsn)
	a := assertAgentModeDefaults(t, d, 0)
	if a.ID == id {
		t.Fatal("custom Auto was converted to builtin Agent")
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM agents WHERE id=$1 AND key='auto' AND NOT builtin`, id); got != 1 {
		t.Fatal("custom Auto was removed or overwritten")
	}
	if got := migrationID(t, d, `SELECT COUNT(*) FROM conversations WHERE agent_key='auto'`); got != 1 {
		t.Fatal("custom Auto's conversation was reassigned")
	}
}
