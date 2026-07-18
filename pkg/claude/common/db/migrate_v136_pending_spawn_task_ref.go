package db

import (
	"database/sql"
	"fmt"
)

// migrateV135toV136 adds task_url / task_label to pending_spawns — the
// per-agent task-reference link a spawn requested via --task. Before this,
// the pending row dropped the link, so a spawn whose enrollment was
// back-filled by the pending-spawn sweeper (delayed conv-id
// materialization) silently lost its task binding even though the spawn
// had reported success (TCL-568). Additive, probe-guarded ADD COLUMNs in
// one transaction (the migrateV110toV111 convention) so a half-applied run
// converges on re-run; legacy rows read as "" = no link, exactly the
// pre-existing no-task case.
func migrateV135toV136(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v135→v136 (pending spawn task ref): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pending_spawns'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v135→v136 (probe pending_spawns): %w", err)
	}
	if haveTable > 0 {
		for _, col := range []string{"task_url", "task_label"} {
			var haveColumn int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = ?`, col,
			).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v135→v136 (probe pending_spawns.%s): %w", col, err)
			}
			if haveColumn == 0 {
				if _, err := tx.Exec(`ALTER TABLE pending_spawns ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("migrate v135→v136 (add pending_spawns.%s): %w", col, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 136`); err != nil {
		return fmt.Errorf("migrate v135→v136 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v135→v136 (commit): %w", err)
	}
	return nil
}
