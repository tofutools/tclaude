package db

import (
	"database/sql"
	"fmt"
)

// migrateV202toV203 preserves the resolved Codex drive while an asynchronous
// spawn waits for its conversation id. NULL belongs to legacy/non-Codex rows;
// both 0 and 1 are authoritative launch selections.
func migrateV202toV203(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v202→v203 (pending Codex drive): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var pendingSpawnsExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pending_spawns'`).Scan(&pendingSpawnsExists); err != nil {
		return fmt.Errorf("migrate v202→v203 (pending Codex drive): probe pending_spawns: %w", err)
	}
	// Some historical self-healing fixtures intentionally contain only the
	// tables needed by the migration under test. Keep the later migration chain
	// convergent for those partial databases; real v202 databases have this
	// table from v59.
	if pendingSpawnsExists != 0 {
		for _, column := range []struct {
			name string
			ddl  string
		}{
			{"codex_app_server", `ALTER TABLE pending_spawns ADD COLUMN codex_app_server INTEGER`},
			{"codex_app_server_source", `ALTER TABLE pending_spawns ADD COLUMN codex_app_server_source TEXT NOT NULL DEFAULT ''`},
			{"codex_state_root", `ALTER TABLE pending_spawns ADD COLUMN codex_state_root TEXT NOT NULL DEFAULT ''`},
			{"codex_state_root_source", `ALTER TABLE pending_spawns ADD COLUMN codex_state_root_source TEXT NOT NULL DEFAULT ''`},
		} {
			var have int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = ?`, column.name).Scan(&have); err != nil {
				return fmt.Errorf("migrate v202→v203 (pending Codex drive): probe %s: %w", column.name, err)
			}
			if have == 0 {
				if _, err := tx.Exec(column.ddl); err != nil {
					return fmt.Errorf("migrate v202→v203 (pending Codex drive): add %s: %w", column.name, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 203`); err != nil {
		return fmt.Errorf("migrate v202→v203 (pending Codex drive): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v202→v203 (pending Codex drive): commit: %w", err)
	}
	return nil
}
