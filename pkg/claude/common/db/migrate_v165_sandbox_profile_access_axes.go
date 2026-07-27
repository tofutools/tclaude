package db

import (
	"database/sql"
	"fmt"
)

// migrateV164toV165 adds the independently optional network and Unix-socket
// rule objects. Empty strings mean absent/unset, preserving every existing
// profile byte-for-byte at the model boundary.
func migrateV164toV165(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v164→v165 (sandbox access axes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v164→v165 (sandbox access axes): probe table: %w", err)
	}
	if haveTable > 0 {
		for _, column := range []string{"network_json", "unix_sockets_json"} {
			var haveColumn int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = ?`,
				column,
			).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v164→v165 (sandbox access axes): probe %s: %w", column, err)
			}
			if haveColumn == 0 {
				if _, err := tx.Exec(
					`ALTER TABLE sandbox_profiles ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`,
				); err != nil {
					return fmt.Errorf("migrate v164→v165 (sandbox access axes): add %s: %w", column, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 165`); err != nil {
		return fmt.Errorf("migrate v164→v165 (sandbox access axes): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v164→v165 (sandbox access axes): commit: %w", err)
	}
	return nil
}
