package db

import (
	"database/sql"
	"fmt"
)

// migrateV342toV343 adds process_agent_dispatches.spawn_lifetime
// (tk_766694fe): how long the agent a dispatch spawned stays ("step", "run"
// or "keep"), resolved and written when it is spawned. Rows from before the
// upgrade stay empty, so the agents earlier runs spawned, which were promised
// to stay, are never retired by a lifetime they did not have.
func migrateV342toV343(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v342→v343 (spawn lifetime): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_agent_dispatches') WHERE name = 'spawn_lifetime'`).Scan(&n); err != nil {
		return fmt.Errorf("migrate v342→v343 (probe): %w", err)
	}
	if n == 0 {
		if _, err := tx.Exec(`ALTER TABLE process_agent_dispatches ADD COLUMN spawn_lifetime TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v342→v343 (add column): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 343`); err != nil {
		return fmt.Errorf("migrate v342→v343 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v342→v343 (commit): %w", err)
	}
	return nil
}
