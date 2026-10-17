package db

import (
	"database/sql"
	"fmt"
)

// migrateV266toV267 lets spawn profiles and groups reference saved prompts the
// way profiles reference roles: an ordered set of saved-prompt names whose
// bodies compose a spawned agent's initial message.
func migrateV266toV267(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v266→v267 (saved prompt refs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, col := range []struct{ table, column string }{
		{"spawn_profiles", "prompt_refs"},
		{"agent_groups", "default_prompts"},
	} {
		// A partial DB healed by an older migration test may lack either table.
		var haveTable, present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, col.table).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v266→v267 (probe %s): %w", col.table, err)
		}
		if haveTable == 0 {
			continue
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('`+col.table+`') WHERE name = ?`, col.column).Scan(&present); err != nil {
			return fmt.Errorf("migrate v266→v267 (inspect %s): %w", col.table, err)
		}
		if present == 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + col.table + ` ADD COLUMN ` + col.column + ` TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return fmt.Errorf("migrate v266→v267 (add %s.%s): %w", col.table, col.column, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 267`); err != nil {
		return fmt.Errorf("migrate v266→v267 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v266→v267 (commit): %w", err)
	}
	return nil
}
