package db

import (
	"database/sql"
	"fmt"
)

// migrateV110toV111 adds immutable, value-based effective sandbox snapshots
// to both enrolled agents and restart-safe pending spawns. Profile IDs and
// assignments are mutable registry metadata; launch and relaunch paths must
// consume the frozen snapshot instead of resolving those references again.
func migrateV110toV111(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v110→v111 (effective sandbox snapshots): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"agents", "pending_spawns"} {
		var haveTable int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v110→v111 (probe %s): %w", table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'effective_sandbox_config'`, table,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v110→v111 (probe %s.effective_sandbox_config): %w", table, err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN effective_sandbox_config TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v110→v111 (add %s.effective_sandbox_config): %w", table, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 111`); err != nil {
		return fmt.Errorf("migrate v110→v111 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v110→v111 (commit): %w", err)
	}
	return nil
}
