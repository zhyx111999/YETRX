package db

import "context"

// SkillEnabledStates contains only explicit global choices. A newly installed
// directory is enabled by default; legacy per-agent bindings remain untouched.
func (d *DB) SkillEnabledStates(ctx context.Context) (map[string]bool, error) {
	rows, err := d.QueryContext(ctx, `SELECT name,enabled FROM skill_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[string]bool)
	for rows.Next() {
		var name string
		var enabled bool
		if err := rows.Scan(&name, &enabled); err != nil {
			return nil, err
		}
		states[name] = enabled
	}
	return states, rows.Err()
}

func (d *DB) SetSkillEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := d.ExecContext(ctx, `INSERT INTO skill_settings(name,enabled) VALUES($1,$2)
		ON CONFLICT(name) DO UPDATE SET enabled=EXCLUDED.enabled`, name, enabled)
	return err
}

func (d *DB) DeleteSkillSettings(ctx context.Context, name string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM skill_settings WHERE name=$1`, name)
	return err
}
