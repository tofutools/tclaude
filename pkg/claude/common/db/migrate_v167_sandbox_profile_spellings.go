package db

import (
	"database/sql"
	"fmt"
)

// migrateV166toV167 adds versioned, non-authority filesystem authoring
// metadata. The empty string is a deliberate legacy sentinel: existing rows
// retain their exact filesystem_json bytes and resolution behavior.
func migrateV166toV167(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v166→v167 (sandbox filesystem spellings): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v166→v167 (sandbox filesystem spellings): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'filesystem_spellings_json'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v166→v167 (sandbox filesystem spellings): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sandbox_profiles ADD COLUMN filesystem_spellings_json TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v166→v167 (sandbox filesystem spellings): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 167`); err != nil {
		return fmt.Errorf("migrate v166→v167 (sandbox filesystem spellings): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v166→v167 (sandbox filesystem spellings): commit: %w", err)
	}
	return nil
}
