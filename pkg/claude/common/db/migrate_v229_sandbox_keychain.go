package db

import (
	"database/sql"
	"fmt"
)

func migrateV228toV229(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v228→v229 (Darwin Keychain opt-out): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'darwin_disable_keychain_write'`).Scan(&haveColumn); err != nil {
		return fmt.Errorf("migrate v228→v229 (probe Darwin Keychain opt-out): %w", err)
	}
	if haveColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN darwin_disable_keychain_write INTEGER NOT NULL DEFAULT 0 CHECK (darwin_disable_keychain_write IN (0, 1))`); err != nil {
			return fmt.Errorf("migrate v228→v229 (add Darwin Keychain opt-out): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 229`); err != nil {
		return fmt.Errorf("migrate v228→v229 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v228→v229 (commit): %w", err)
	}
	return nil
}
