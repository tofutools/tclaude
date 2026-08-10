package db

import (
	"database/sql"
	"fmt"
)

func migrateV206toV207(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v206→v207 (Codex native profile ownership): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, column := range []struct{ name, sql string }{
		{"owner_agent_id", `ALTER TABLE codex_native_permission_profiles ADD COLUMN owner_agent_id TEXT NOT NULL DEFAULT ''`},
		{"owner_conv_id", `ALTER TABLE codex_native_permission_profiles ADD COLUMN owner_conv_id TEXT NOT NULL DEFAULT ''`},
		{"launch_id", `ALTER TABLE codex_native_permission_profiles ADD COLUMN launch_id TEXT NOT NULL DEFAULT ''`},
		{"launch_ready", `ALTER TABLE codex_native_permission_profiles ADD COLUMN launch_ready INTEGER NOT NULL DEFAULT 0 CHECK (launch_ready IN (0, 1))`},
	} {
		if err := addColumnIfMissing(tx, "codex_native_permission_profiles", column.name, column.sql); err != nil {
			return fmt.Errorf("migrate v206→v207 (Codex native profile ownership): %w", err)
		}
	}
	for _, stmt := range []string{
		`UPDATE codex_native_permission_profiles SET
			owner_agent_id = CASE WHEN owner_agent_id = '' THEN COALESCE((SELECT agent_id FROM codex_app_server_runtimes r WHERE r.generation = codex_native_permission_profiles.generation), '') ELSE owner_agent_id END,
			owner_conv_id = CASE WHEN owner_conv_id = '' THEN COALESCE((SELECT conv_id FROM codex_app_server_runtimes r WHERE r.generation = codex_native_permission_profiles.generation), '') ELSE owner_conv_id END,
			launch_id = CASE WHEN launch_id = '' THEN COALESCE((SELECT launch_id FROM codex_app_server_runtimes r WHERE r.generation = codex_native_permission_profiles.generation), '') ELSE launch_id END,
			launch_ready = CASE WHEN EXISTS (SELECT 1 FROM codex_app_server_runtimes r WHERE r.generation = codex_native_permission_profiles.generation)
				THEN COALESCE((SELECT state = 'ready' FROM codex_app_server_runtimes r WHERE r.generation = codex_native_permission_profiles.generation), launch_ready)
				ELSE launch_ready END`,
		`UPDATE schema_version SET version = 207`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v206→v207 (Codex native profile ownership): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v206→v207 (Codex native profile ownership): commit: %w", err)
	}
	return nil
}
