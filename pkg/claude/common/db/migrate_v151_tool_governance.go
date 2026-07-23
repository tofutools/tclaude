package db

import (
	"database/sql"
	"fmt"
)

// migrateV150toV151 adds the optional OpenCode tool-governance field to saved
// spawn profiles and roles. Empty preserves legacy rows; the daemon resolves
// it to OpenCode's backward-compatible allow default at launch.
func migrateV150toV151(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v150→v151 (tool governance): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, target := range []struct {
		table, probeSQL, alterSQL string
	}{
		{"spawn_profiles", `SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'tools'`,
			`ALTER TABLE spawn_profiles ADD COLUMN tools TEXT NOT NULL DEFAULT ''`},
		{"roles", `SELECT COUNT(*) FROM pragma_table_info('roles') WHERE name = 'tools'`,
			`ALTER TABLE roles ADD COLUMN tools TEXT NOT NULL DEFAULT ''`},
	} {
		haveTable, err := txTableExists(tx, target.table)
		if err != nil {
			return fmt.Errorf("migrate v150→v151 (probe %s): %w", target.table, err)
		}
		if !haveTable {
			continue
		}
		var have int
		if err := tx.QueryRow(target.probeSQL).Scan(&have); err != nil {
			return fmt.Errorf("migrate v150→v151 (probe %s.tools): %w", target.table, err)
		}
		if have == 0 {
			if _, err := tx.Exec(target.alterSQL); err != nil {
				return fmt.Errorf("migrate v150→v151 (add %s.tools): %w", target.table, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 151`); err != nil {
		return fmt.Errorf("migrate v150→v151 (version): %w", err)
	}
	return tx.Commit()
}
