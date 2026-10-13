package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/minimalprompt"
)

func TestNativeAgentConflicts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		targetBuiltin bool
		withAuto      bool
	}{
		{"custom agent", false, false},
		{"custom agent with builtin auto", false, true},
		{"both builtins", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, dsn := nativeMigrationDB(t, true)
			nativeExec(t, d, `INSERT INTO agents(key,name,role,builtin) VALUES ('agent','Keep target','assistant',$1)`, tc.targetBuiltin)
			if tc.withAuto {
				nativeExec(t, d, `INSERT INTO agents(key,name,role) VALUES ('auto','Auto','assistant')`)
			}
			nativeExec(t, d, `INSERT INTO agent_prompts(agent_id,version,template_text) SELECT id,1,'keep prompt' FROM agents`)
			nativeExec(t, d, `UPDATE agents SET current_prompt_id=(SELECT id FROM agent_prompts WHERE agent_id=agents.id)`)
			nativeExec(t, d, `INSERT INTO conversations(agent_key,title) VALUES ('auto','keep conversation')`)
			nativeExec(t, d, `INSERT INTO tools(key,agents) VALUES ('kept','["auto","agent","worker"]')`)
			queries := []string{`SELECT * FROM agents ORDER BY id`, `SELECT * FROM agent_prompts ORDER BY id`, `SELECT * FROM conversations`, `SELECT * FROM tools`, `SELECT * FROM settings`}
			before := make([]string, len(queries))
			for i, q := range queries {
				before[i] = nativeSnapshot(t, d, q)
			}
			opened, err := Open(dsn)
			if opened != nil {
				opened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "agent key conflict") {
				t.Fatalf("expected explicit conflict, got %v", err)
			}
			for i, q := range queries {
				if got := nativeSnapshot(t, d, q); got != before[i] {
					t.Errorf("conflict changed data: %s", q)
				}
			}
		})
	}
}

func TestNativeAgentRollbackAndRetry(t *testing.T) {
	for _, pi := range []bool{false, true} {
		t.Run(fmt.Sprintf("pi=%t", pi), func(t *testing.T) {
			d, dsn := nativeMigrationDB(t, true)
			nativeLegacyFixture(t, d, pi, false)
			queries := []string{
				`SELECT * FROM agents`, `SELECT * FROM agent_prompts ORDER BY id`,
				`SELECT * FROM conversations ORDER BY id`, `SELECT * FROM agent_triggers ORDER BY id`,
				`SELECT * FROM tools ORDER BY key`, `SELECT * FROM settings ORDER BY key`,
				`SELECT * FROM agent_visibility ORDER BY resource_id`, `SELECT * FROM agent_skill_visibility ORDER BY skill_name`,
			}
			before := make([]string, len(queries))
			for i, q := range queries {
				before[i] = nativeSnapshot(t, d, q)
			}
			// Fail the last reference update, after the identity and new prompt have
			// been written. Earlier writes and the marker must roll back together.
			nativeExec(t, d, `CREATE FUNCTION reject_native_migration() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'injected native migration failure'; END $$;
CREATE TRIGGER reject_native_migration BEFORE UPDATE ON tools FOR EACH ROW EXECUTE FUNCTION reject_native_migration()`)
			opened, err := Open(dsn)
			if opened != nil {
				opened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "injected native migration failure") {
				t.Fatalf("expected injected failure, got %v", err)
			}
			for i, q := range queries {
				if got := nativeSnapshot(t, d, q); got != before[i] {
					t.Errorf("transaction leaked writes: %s\nbefore %s\nafter %s", q, before[i], got)
				}
			}
			nativeExec(t, d, `DROP TRIGGER reject_native_migration ON tools`)
			nativeOpen(t, dsn)
			if n := nativeInt(t, d, `SELECT count(*) FROM agent_prompts WHERE agent_id=5`); n != 4 {
				t.Fatalf("retry lost or duplicated prompt versions: %d", n)
			}
			if n := nativeInt(t, d, `SELECT count(*) FROM settings WHERE key='native_agent_v1' AND value='done'`); n != 1 {
				t.Fatal("successful retry did not mark completion")
			}
		})
	}
}

func TestNativeAgentCustomAutoIsPreserved(t *testing.T) {
	d, dsn := nativeMigrationDB(t, true)
	nativeLegacyFixture(t, d, false, true)
	nativeExec(t, d, `UPDATE agents SET builtin=false WHERE id=5`)
	queries := []string{
		`SELECT * FROM agents WHERE id=5`, `SELECT * FROM agent_prompts WHERE agent_id=5 ORDER BY id`,
		`SELECT * FROM conversations ORDER BY id`, `SELECT * FROM agent_triggers ORDER BY id`, `SELECT * FROM tools ORDER BY key`,
	}
	before := make([]string, len(queries))
	for i, q := range queries {
		before[i] = nativeSnapshot(t, d, q)
	}
	nativeOpen(t, dsn)
	for i, q := range queries {
		if got := nativeSnapshot(t, d, q); got != before[i] {
			t.Errorf("custom auto changed: %s", q)
		}
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM agents WHERE key='agent' AND builtin AND id<>5`); n != 1 {
		t.Fatal("native builtin was not created separately")
	}
}

func TestNativeAgentPromptMatching(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, author string
	}{
		{"pi edited by system", legacyPiPrompt + "\n", "system"},
		{"auto edited by system", legacyAutoPrompt + " ", "system"},
		{"pi saved by user", legacyPiPrompt, "owner"},
		{"auto saved by user", legacyAutoPrompt, "owner"},
		{"unknown author", legacyPiPrompt, ""},
		{"empty user prompt", "", "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, dsn := nativeMigrationDB(t, true)
			nativeExec(t, d, `INSERT INTO agents(id,key,name,role,run_seconds) VALUES (5,'agent','Agent','assistant',1200)`)
			nativeExec(t, d, `SELECT setval('agents_id_seq',5)`)
			if _, err := d.SavePrompt(5, tc.prompt, "keep", tc.author); err != nil {
				t.Fatal(err)
			}
			before := nativeSnapshot(t, d, `SELECT * FROM agent_prompts`)
			nativeOpen(t, dsn)
			if got := nativeSnapshot(t, d, `SELECT * FROM agent_prompts`); got != before {
				t.Fatal("non-default / user-authored prompt was replaced")
			}
			if n := nativeInt(t, d, `SELECT run_seconds FROM agents WHERE id=5`); n != 1200 {
				t.Fatal("existing Agent's edited budget was changed")
			}
		})
	}
}

func TestNativeAgentConcurrentMigrationAndPromptSaves(t *testing.T) {
	d, _ := nativeMigrationDB(t, true)
	nativeLegacyFixture(t, d, true, false)
	const writers = 8
	nativeParallel(t, writers*3, func(i int) error {
		switch i % 3 {
		case 0:
			return d.migrateDefaultAgent()
		case 1:
			_, err := d.SavePrompt(5, "concurrent user edit", "keep", "owner")
			return err
		default:
			return d.SeedPromptIfEmpty(5, minimalprompt.Default)
		}
	})
	if current, err := d.CurrentPrompt(5); err != nil || current != "concurrent user edit" {
		t.Fatalf("migration/seed overwrote concurrent user save: %q, %v", current, err)
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM agent_prompts WHERE agent_id=5 AND template_text='concurrent user edit'`); n != writers {
		t.Fatalf("lost concurrent prompt saves: %d want %d", n, writers)
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM agent_prompts WHERE agent_id=5 AND template_text=$1`, minimalprompt.Default); n > 1 {
		t.Fatalf("duplicate migration versions: %d", n)
	}
	if n := nativeInt(t, d, `SELECT count(*) FROM settings WHERE key='native_agent_v1' AND value='done'`); n != 1 {
		t.Fatal("concurrent migration did not complete")
	}
}

func TestNativeAgentConcurrentFirstPromptSeed(t *testing.T) {
	d, _ := nativeMigrationDB(t, true)
	id := nativeInt(t, d, `INSERT INTO agents(key,name,role,builtin) VALUES ('review','Review','assistant',false) RETURNING id`)
	nativeParallel(t, 16, func(int) error { return d.SeedPromptIfEmpty(id, "default") })
	if n := nativeInt(t, d, `SELECT count(*) FROM agent_prompts WHERE agent_id=$1`, id); n != 1 {
		t.Fatalf("first seed created %d versions", n)
	}
	// A first user save racing the seed also wins, regardless of lock order.
	id = nativeInt(t, d, `INSERT INTO agents(key,name,role,builtin) VALUES ('fresh','Fresh','assistant',false) RETURNING id`)
	nativeParallel(t, 16, func(i int) error {
		if i == 0 {
			_, err := d.SavePrompt(id, "first user edit", "keep", "owner")
			return err
		}
		return d.SeedPromptIfEmpty(id, "default")
	})
	if current, err := d.CurrentPrompt(id); err != nil || current != "first user edit" {
		t.Fatalf("seed overwrote first user edit: %q, %v", current, err)
	}
}
