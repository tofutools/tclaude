package db

import (
	"database/sql"
	"fmt"
)

// migrateV224toV225 adds the optional temporary-filesystem payload (TCL-1218).
//
// '[]' is the exact opt-out default: a legacy profile decodes to no tmpfs rows,
// which renders no mount-plan entry at all, so its launch command stays
// byte-identical to what it produced before this column existed.
func migrateV224toV225(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v224→v225 (sandbox tmpfs mounts): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v224→v225 (probe sandbox_profiles): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'tmpfs_json'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v224→v225 (probe tmpfs mounts): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN tmpfs_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return fmt.Errorf("migrate v224→v225 (add tmpfs mounts): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 225`); err != nil {
		return fmt.Errorf("migrate v224→v225 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v224→v225 (commit): %w", err)
	}
	return nil
}
