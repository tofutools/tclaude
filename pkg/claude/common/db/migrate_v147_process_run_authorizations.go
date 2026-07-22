package db

import (
	"database/sql"
	"fmt"
)

// migrateV146toV147 keeps the daemon's explicit per-run program authorization
// beside the pinned template and checkpoint. It is immutable run input, not a
// second lifecycle record. Existing pre-integration rows fail closed with no
// authorized profiles.
func migrateV146toV147(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v146→v147 (process authorization): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_runs') WHERE name = 'program_authorizations_json'`).Scan(&present); err != nil {
		return fmt.Errorf("migrate v146→v147 (inspect process_runs): %w", err)
	}
	if present == 0 {
		if _, err := tx.Exec(`ALTER TABLE process_runs ADD COLUMN program_authorizations_json TEXT NOT NULL DEFAULT '[]'
			CHECK(length(CAST(program_authorizations_json AS BLOB)) BETWEEN 2 AND 262144)`); err != nil {
			return fmt.Errorf("migrate v146→v147 (add process authorization): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 147`); err != nil {
		return fmt.Errorf("migrate v146→v147 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v146→v147 (commit): %w", err)
	}
	return nil
}
