package db

import (
	"database/sql"
	"fmt"
)

// migrateV219toV220 adds the harness-config access posture to sandbox
// profiles.
//
// The empty default deliberately does NOT preserve prior behavior: an omitted
// value means the read-only floor over the harness's own config surface
// applies. Existing profiles therefore gain the floor on their next launch,
// which is the point of the change — an operator who needs the old writable
// posture authors harness_config: "write" explicitly.
func migrateV219toV220(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v219→v220 (sandbox harness config): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v219→v220 (sandbox harness config): probe table: %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'harness_config'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v219→v220 (sandbox harness config): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN harness_config TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v219→v220 (sandbox harness config): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 220`); err != nil {
		return fmt.Errorf("migrate v219→v220 (sandbox harness config): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v219→v220 (sandbox harness config): commit: %w", err)
	}
	return nil
}
