package db

import (
	"database/sql"
	"fmt"
)

// migrateV279toV280 lets usage profiles link to a group template: the
// usage_profile_assignments scope CHECK gains 'template' (SQLite rebuilds
// the table to change a CHECK), and deleting a template drops its links. A
// group deployed from the template takes them as its group layer.
func migrateV279toV280(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v279→v280 (usage template scope): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The cleanup triggers on other tables name this table, and SQLite's
	// rename re-checks them: drop them for the rebuild, then recreate them.
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS usage_profile_assignments_group_gone`,
		`DROP TRIGGER IF EXISTS usage_profile_assignments_spawn_profile_gone`,
		`DROP TRIGGER IF EXISTS usage_profile_assignments_agent_gone`,
		`CREATE TABLE usage_profile_assignments_v280 (
			scope      TEXT NOT NULL CHECK (scope IN ('global', 'group', 'agent', 'spawn_profile', 'template')),
			scope_id   TEXT NOT NULL DEFAULT '',
			profile_id INTEGER NOT NULL REFERENCES usage_profiles(id) ON DELETE CASCADE,
			source     TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			PRIMARY KEY (scope, scope_id, profile_id)
		) STRICT`,
		`INSERT INTO usage_profile_assignments_v280 (scope, scope_id, profile_id, source, created_at)
			SELECT scope, scope_id, profile_id, source, created_at FROM usage_profile_assignments`,
		`DROP TABLE usage_profile_assignments`,
		`ALTER TABLE usage_profile_assignments_v280 RENAME TO usage_profile_assignments`,
		`CREATE INDEX IF NOT EXISTS idx_usage_profile_assignments_profile
			ON usage_profile_assignments(profile_id)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v279→v280 (usage template scope): %w", err)
		}
	}
	for _, trigger := range []struct{ table, scope, id string }{
		{"agent_groups", "group", "CAST(OLD.id AS TEXT)"},
		{"spawn_profiles", "spawn_profile", "CAST(OLD.id AS TEXT)"},
		{"agents", "agent", "OLD.agent_id"},
		{"group_templates", "template", "CAST(OLD.id AS TEXT)"},
	} {
		// A partial DB healed by an older migration test may lack the table.
		var haveTable int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, trigger.table).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v279→v280 (probe %s): %w", trigger.table, err)
		}
		if haveTable == 0 {
			continue
		}
		name := "usage_profile_assignments_" + trigger.scope + "_gone"
		if _, err := tx.Exec(fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s
			AFTER DELETE ON %s BEGIN
				DELETE FROM usage_profile_assignments
				WHERE scope = '%s' AND scope_id = %s;
			END`, name, trigger.table, trigger.scope, trigger.id)); err != nil {
			return fmt.Errorf("migrate v279→v280 (usage template scope): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 280`); err != nil {
		return fmt.Errorf("migrate v279→v280 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v279→v280 (commit): %w", err)
	}
	return nil
}
