package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

// UpgradePromptDefault replaces only an exact shipped default. The agent lock
// is shared with SavePrompt, so a concurrent user edit always wins or follows it.
func (d *DB) UpgradePromptDefault(key, previousSHA256, template string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var current sql.NullInt64
	if err := tx.QueryRow(`SELECT id,current_prompt_id FROM agents WHERE key=$1 FOR UPDATE`, key).Scan(&id, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !current.Valid {
		return nil
	}
	var text string
	if err := tx.QueryRow(`SELECT template_text FROM agent_prompts WHERE id=$1`, current.Int64).Scan(&text); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(text))
	if hex.EncodeToString(digest[:]) != previousSHA256 {
		return nil
	}
	if _, err := savePrompt(tx, id, template, "后台任务生命周期默认说明更新", "system"); err != nil {
		return err
	}
	return tx.Commit()
}
