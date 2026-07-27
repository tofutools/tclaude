package db

import (
	"database/sql"
	"fmt"
)

// migrateV161toV162 records the exact outer-layer authority required to
// reproduce an agentd-owned OpenCode server restart. Legacy rows receive the
// truthful harness-builtin implementation and no launch spec.
func migrateV161toV162(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v161→v162 (OpenCode runtime sandbox): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_runtimes'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v161→v162 (OpenCode runtime sandbox): probe table: %w", err)
	}
	if haveTable > 0 {
		for _, column := range []struct {
			name       string
			definition string
		}{
			{
				name:       "sandbox_implementation",
				definition: "TEXT NOT NULL DEFAULT 'harness-builtin'",
			},
			{
				name:       "sandbox_launch_spec_json",
				definition: "TEXT NOT NULL DEFAULT ''",
			},
		} {
			var haveColumn int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('opencode_runtimes') WHERE name = ?`,
				column.name,
			).Scan(&haveColumn); err != nil {
				return fmt.Errorf(
					"migrate v161→v162 (OpenCode runtime sandbox): probe %s: %w",
					column.name, err)
			}
			if haveColumn == 0 {
				query := fmt.Sprintf(
					"ALTER TABLE opencode_runtimes ADD COLUMN %s %s",
					column.name, column.definition)
				if _, err := tx.Exec(query); err != nil {
					return fmt.Errorf(
						"migrate v161→v162 (OpenCode runtime sandbox): add %s: %w",
						column.name, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 162`); err != nil {
		return fmt.Errorf("migrate v161→v162 (OpenCode runtime sandbox): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v161→v162 (OpenCode runtime sandbox): commit: %w", err)
	}
	return nil
}
