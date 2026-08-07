package db

import (
	"database/sql"
	"fmt"
)

// migrateV188toV189 adds the optional pre-launch script blocks payload
// (TCL-1039).
//
// '[]' is the exact opt-out default: a legacy profile decodes to no blocks,
// which renders no shell fragment at all, so its launch command stays
// byte-identical to what it produced before this column existed.
func migrateV188toV189(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v188→v189 (sandbox pre-launch blocks): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v188→v189 (probe sandbox_profiles): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'pre_launch_json'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v188→v189 (probe pre-launch blocks): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN pre_launch_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return fmt.Errorf("migrate v188→v189 (add pre-launch blocks): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 189`); err != nil {
		return fmt.Errorf("migrate v188→v189 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v188→v189 (commit): %w", err)
	}
	return nil
}
