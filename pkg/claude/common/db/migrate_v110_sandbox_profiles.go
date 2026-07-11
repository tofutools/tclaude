package db

import (
	"database/sql"
	"fmt"
)

// migrateV109toV110 creates the durable sandbox-profile registry and its two
// assignment surfaces. Human-facing names are retained as snapshots while the
// companion IDs are authoritative, matching the stable spawn-profile pattern.
func migrateV109toV110(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v109→v110 (sandbox profiles): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS sandbox_profiles (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL UNIQUE,
			filesystem_json  TEXT NOT NULL DEFAULT '[]',
			environment_json TEXT NOT NULL DEFAULT '[]',
			created_at       TEXT NOT NULL,
			updated_at       TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sandbox_profile_global_assignment (
			id           INTEGER PRIMARY KEY CHECK (id = 1),
			profile_name TEXT NOT NULL,
			profile_id   INTEGER NOT NULL REFERENCES sandbox_profiles(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("migrate v109→v110 (sandbox profiles): create registry: %w", err)
	}

	haveGroups, err := txTableExists(tx, "agent_groups")
	if err != nil {
		return fmt.Errorf("migrate v109→v110 (sandbox profiles): probe agent_groups: %w", err)
	}
	if haveGroups {
		for _, column := range []struct{ name, ddl string }{
			{"sandbox_profile", "TEXT NOT NULL DEFAULT ''"},
			{"sandbox_profile_id", "INTEGER"},
		} {
			var have int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = ?`, column.name).Scan(&have); err != nil {
				return fmt.Errorf("migrate v109→v110 (sandbox profiles): probe agent_groups.%s: %w", column.name, err)
			}
			if have == 0 {
				if _, err := tx.Exec("ALTER TABLE agent_groups ADD COLUMN " + column.name + " " + column.ddl); err != nil {
					return fmt.Errorf("migrate v109→v110 (sandbox profiles): add agent_groups.%s: %w", column.name, err)
				}
			}
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_groups_sandbox_profile_id ON agent_groups(sandbox_profile_id)`); err != nil {
			return fmt.Errorf("migrate v109→v110 (sandbox profiles): group index: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 110`); err != nil {
		return fmt.Errorf("migrate v109→v110 (sandbox profiles): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v109→v110 (sandbox profiles): commit: %w", err)
	}
	return nil
}
