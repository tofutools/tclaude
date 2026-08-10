package db

import (
	"database/sql"
	"fmt"
)

// migrateV205toV206 adds the single operator-selected group used to break an
// otherwise ambiguous directory auto-join. It also converges the short-lived
// pre-rebase feature-v205 schema: that build stamped v205 after adding this
// column, while upstream v205 independently added the Codex native-profile
// table. Ensuring both shapes here prevents either v205 history from skipping
// the other's DDL. The partial unique index makes the group invariant durable
// even for callers outside the normal atomic setter.
func migrateV205toV206(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v205→v206 (default spawn group): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS codex_native_permission_profiles (
		generation   TEXT PRIMARY KEY,
		profile_name TEXT NOT NULL UNIQUE,
		profile_toml TEXT NOT NULL,
		cleanup_pending INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_pending IN (0, 1)),
		created_at   INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("migrate v205→v206 (default spawn group): heal Codex native profiles: %w", err)
	}
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_groups'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v205→v206 (default spawn group): table probe: %w", err)
	}
	var haveColumn int
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_spawn_group'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v205→v206 (default spawn group): column probe: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE agent_groups ADD COLUMN default_spawn_group INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("migrate v205→v206 (default spawn group): add column: %w", err)
			}
		}
		if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_groups_one_default_spawn
			ON agent_groups(default_spawn_group) WHERE default_spawn_group = 1`); err != nil {
			return fmt.Errorf("migrate v205→v206 (default spawn group): index: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 206`); err != nil {
		return fmt.Errorf("migrate v205→v206 (default spawn group): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v205→v206 (default spawn group): commit: %w", err)
	}
	return nil
}
