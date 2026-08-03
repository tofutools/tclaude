package db

import (
	"database/sql"
	"fmt"
)

func migrateV186toV187(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v186→v187 (Darwin mach-register): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'darwin_allow_mach_register'`).Scan(&haveColumn); err != nil {
		return fmt.Errorf("migrate v186→v187 (probe Darwin mach-register): %w", err)
	}
	if haveColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN darwin_allow_mach_register INTEGER NOT NULL DEFAULT 0 CHECK (darwin_allow_mach_register IN (0, 1))`); err != nil {
			return fmt.Errorf("migrate v186→v187 (add Darwin mach-register): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 187`); err != nil {
		return fmt.Errorf("migrate v186→v187 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v186→v187 (commit): %w", err)
	}
	return nil
}
