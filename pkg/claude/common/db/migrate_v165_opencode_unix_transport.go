package db

import (
	"database/sql"
	"fmt"
)

// migrateV164toV165 adds the replay authority for the tclaude-owned OpenCode
// Unix relay. Existing runtimes remain explicitly loopback-backed; this
// migration enables no posture and moves no live endpoint.
func migrateV164toV165(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v164→v165 (OpenCode Unix transport): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_runtimes'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v164→v165 (OpenCode Unix transport): probe table: %w", err)
	}
	if haveTable > 0 {
		for _, column := range []struct {
			name       string
			definition string
		}{
			{name: "transport", definition: "TEXT NOT NULL DEFAULT 'loopback-tcp'"},
			{name: "control_socket_path", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "control_socket_device", definition: "INTEGER NOT NULL DEFAULT 0"},
			{name: "control_socket_inode", definition: "INTEGER NOT NULL DEFAULT 0"},
		} {
			var haveColumn int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('opencode_runtimes') WHERE name = ?`,
				column.name,
			).Scan(&haveColumn); err != nil {
				return fmt.Errorf(
					"migrate v164→v165 (OpenCode Unix transport): probe %s: %w",
					column.name, err)
			}
			if haveColumn == 0 {
				query := fmt.Sprintf(
					"ALTER TABLE opencode_runtimes ADD COLUMN %s %s",
					column.name, column.definition)
				if _, err := tx.Exec(query); err != nil {
					return fmt.Errorf(
						"migrate v164→v165 (OpenCode Unix transport): add %s: %w",
						column.name, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 165`); err != nil {
		return fmt.Errorf("migrate v164→v165 (OpenCode Unix transport): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v164→v165 (OpenCode Unix transport): commit: %w", err)
	}
	return nil
}
