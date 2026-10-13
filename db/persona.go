package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Persona is a globally selectable prompt persona. Its prompt is stored and
// returned verbatim; runtime injection is deliberately outside this module.
type Persona struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Prompt    string    `json:"prompt"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	ErrPersonaNotFound   = errors.New("persona not found")
	ErrPersonaNameExists = errors.New("persona name already exists")
)

// This lock serializes activation and deletion of the active row across server
// instances. The partial unique index remains the database invariant for every
// writer, including writers outside this package.
const personaActivationLockKey int64 = 7337741005

const personaColumns = `id,name,prompt,is_active,created_at,updated_at`

func scanPersona(sc interface{ Scan(...any) error }) (*Persona, error) {
	var p Persona
	err := sc.Scan(&p.ID, &p.Name, &p.Prompt, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

// ListPersonas returns every persona in stable creation order.
func (d *DB) ListPersonas() ([]*Persona, error) {
	rows, err := d.Query(`SELECT ` + personaColumns + ` FROM personas ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Persona, 0)
	for rows.Next() {
		p, err := scanPersona(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PersonaByID returns one persona, or nil when the id does not exist.
func (d *DB) PersonaByID(id int64) (*Persona, error) {
	p, err := scanPersona(d.QueryRow(`SELECT `+personaColumns+` FROM personas WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ActivePersona returns the global active persona, or nil when no persona is
// active. The partial unique index makes LIMIT 1 a defensive convenience rather
// than the source of the invariant.
func (d *DB) ActivePersona() (*Persona, error) {
	p, err := scanPersona(d.QueryRow(`SELECT ` + personaColumns + ` FROM personas WHERE is_active LIMIT 1`))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// SavePersona inserts a persona when ID is zero, otherwise updates its name and
// prompt. Saving never changes activation; callers use SetActivePersona for that
// separate state transition.
func (d *DB) SavePersona(p *Persona) (int64, error) {
	if p == nil {
		return 0, errors.New("persona is nil")
	}
	if p.ID == 0 {
		var id int64
		err := d.QueryRow(`
INSERT INTO personas(name,prompt)
VALUES ($1,$2)
RETURNING id`, p.Name, p.Prompt).Scan(&id)
		return id, personaSaveError(err)
	}

	res, err := d.Exec(`UPDATE personas SET name=$1,prompt=$2 WHERE id=$3`, p.Name, p.Prompt, p.ID)
	if err != nil {
		return 0, personaSaveError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrPersonaNotFound
	}
	return p.ID, nil
}

func personaSaveError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_personas_name" {
		return ErrPersonaNameExists
	}
	return err
}

// SetActivePersona makes exactly one existing persona active. The transaction
// advisory lock serializes concurrent activations even when there was no active
// row to lock; the unique partial index protects the invariant independently.
func (d *DB) SetActivePersona(id int64) error {
	return d.SetActivePersonaContext(context.Background(), id)
}

func (d *DB) SetActivePersonaContext(ctx context.Context, id int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, personaActivationLockKey); err != nil {
		return err
	}
	var lockedID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM personas WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID); err != nil {
		if err == sql.ErrNoRows {
			return ErrPersonaNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personas SET is_active=false WHERE is_active`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personas SET is_active=true WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePersona removes a persona. Deleting an active persona intentionally
// leaves no active persona, and never promotes another row implicitly.
func (d *DB) DeletePersona(id int64) error {
	_, err := d.DeletePersonaContext(context.Background(), id)
	return err
}

// DeletePersonaContext reports whether the deleted row was active so an API can
// make the clearing behavior explicit without a racy pre-delete read.
func (d *DB) DeletePersonaContext(ctx context.Context, id int64) (activeCleared bool, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, personaActivationLockKey); err != nil {
		return false, err
	}
	var isActive bool
	if err := tx.QueryRowContext(ctx, `SELECT is_active FROM personas WHERE id=$1 FOR UPDATE`, id).Scan(&isActive); err != nil {
		if err == sql.ErrNoRows {
			return false, ErrPersonaNotFound
		}
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM personas WHERE id=$1`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return isActive, nil
}
