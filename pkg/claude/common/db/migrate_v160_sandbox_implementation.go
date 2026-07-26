package db

import (
	"database/sql"
	"fmt"
)

// migrateV159toV160 records which implementation owns launch confinement.
// Legacy rows receive harness-builtin, exactly the behavior they ran with.
func migrateV159toV160(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v159→v160 (sandbox implementation): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions')
		WHERE name = 'sandbox_implementation'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v159→v160 (sandbox implementation): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN sandbox_implementation TEXT NOT NULL DEFAULT 'harness-builtin'`); err != nil {
			return fmt.Errorf("migrate v159→v160 (sandbox implementation): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 160`); err != nil {
		return fmt.Errorf("migrate v159→v160 (sandbox implementation): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v159→v160 (sandbox implementation): commit: %w", err)
	}
	return nil
}
