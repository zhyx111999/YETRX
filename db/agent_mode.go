package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/Autumn-27/artex/minimalprompt"
)

// migrateDefaultAgent runs before the other seeds. Renaming the old row keeps
// its ID, LLM profile and prompt history. The new key itself is the completion
// marker: the entire change commits atomically, and later starts never reseed an
// existing builtin Agent's settings, bindings or prompt.
func (d *DB) migrateDefaultAgent() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Also protect the absent-key case from concurrent inserts. Open already
	// serializes startup with the schema lock; this lock covers runtime writers
	// and direct seed calls without acquiring that session lock a second time.
	if _, err := tx.Exec(`LOCK TABLE agents IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	type candidate struct {
		id      int64
		builtin bool
	}
	lookup := func(key string) (candidate, error) {
		var a candidate
		err := tx.QueryRow(`SELECT id, builtin FROM agents WHERE key=$1`, key).Scan(&a.id, &a.builtin)
		if errors.Is(err, sql.ErrNoRows) {
			return candidate{}, nil
		}
		return a, err
	}
	current, err := lookup(minimalprompt.AgentKey)
	if err != nil {
		return err
	}
	if current.id != 0 && !current.builtin {
		return fmt.Errorf("agent key conflict: non-builtin agent %q (id=%d) already exists; rename its key before retrying", minimalprompt.AgentKey, current.id)
	}
	legacy, err := lookup("auto")
	if err != nil {
		return err
	}
	if current.id != 0 && legacy.builtin {
		return fmt.Errorf("agent key conflict: builtin auto (id=%d) and agent (id=%d) both exist; resolve without discarding either history before retrying", legacy.id, current.id)
	}

	if current.id == 0 {
		agentID := legacy.id
		if legacy.builtin {
			_, err = tx.Exec(`UPDATE agents SET key=$1, name=$2, description=$3,
role='assistant', builtin=true, enabled=true, max_turns=0, run_seconds=0,
interactive_shell=false, web_search=false WHERE id=$4`,
				minimalprompt.AgentKey, minimalprompt.AgentName, minimalprompt.AgentDescription, agentID)
		} else {
			err = tx.QueryRow(`INSERT INTO agents
(key, name, description, role, builtin, enabled, max_turns, run_seconds, interactive_shell, web_search)
VALUES ($1, $2, $3, 'assistant', true, true, 0, 0, false, false) RETURNING id`,
				minimalprompt.AgentKey, minimalprompt.AgentName, minimalprompt.AgentDescription).Scan(&agentID)
		}
		if err != nil {
			return err
		}

		if legacy.builtin {
			// ID-based visibility would otherwise silently carry Auto's tools and
			// skills (including api-recon) into Agent. Retain every binding row,
			// but deactivate it; later explicit user edits are left alone.
			if _, err := tx.Exec(`UPDATE agent_visibility SET enabled=false WHERE agent_id=$1 AND enabled`, agentID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE agent_skill_visibility SET enabled=false WHERE agent_id=$1 AND enabled`, agentID); err != nil {
				return err
			}
		}

		// Archiving means leaving all old versions (including user edits) intact
		// and moving only the current pointer to a newly appended version.
		var promptID int64
		if err := tx.QueryRow(`INSERT INTO agent_prompts (agent_id, version, template_text, note, updated_by)
SELECT $1, COALESCE(MAX(version), 0)+1, $2, 'Agent: pi native tools and workflow', 'system'
FROM agent_prompts WHERE agent_id=$1 RETURNING id`, agentID, minimalprompt.Default).Scan(&promptID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, promptID, agentID); err != nil {
			return err
		}
	}

	// A custom agent that happens to be named auto is not the retired builtin.
	// With no such custom agent, historical Auto conversations keep their IDs,
	// activities and LLM pins and continue under Agent.
	if legacy.id == 0 || legacy.builtin {
		if _, err := tx.Exec(`UPDATE conversations SET agent_key=$1 WHERE agent_key='auto'`, minimalprompt.AgentKey); err != nil {
			return err
		}
	}
	// tools.agents JSON and agent_triggers retain their old keys and settings.
	// Rewriting them would silently enable platform behavior under the new mode.
	return tx.Commit()
}
