package db

import (
	"database/sql"
	"fmt"
)

// migrateV200toV201 adds the opt-in Codex app-server profile field and the
// private runtime identity registry. Existing profiles remain NULL, therefore
// on the legacy send-keys path.
func migrateV200toV201(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v200→v201 (Codex app-server runtime): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'codex_app_server'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v200→v201 (Codex app-server runtime): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN codex_app_server INTEGER`); err != nil {
			return fmt.Errorf("migrate v200→v201 (Codex app-server runtime): add profile column: %w", err)
		}
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS codex_app_server_runtimes (
			generation TEXT PRIMARY KEY,
			launch_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			conv_id TEXT NOT NULL DEFAULT '',
			thread_id TEXT NOT NULL DEFAULT '',
			socket_path TEXT NOT NULL UNIQUE,
			server_pid INTEGER NOT NULL DEFAULT 0,
			codex_version TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_codex_app_server_runtime_conv
			ON codex_app_server_runtimes(conv_id);
		CREATE INDEX IF NOT EXISTS idx_codex_app_server_runtime_agent
			ON codex_app_server_runtimes(agent_id);
	`); err != nil {
		return fmt.Errorf("migrate v200→v201 (Codex app-server runtime): create runtime table: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 201`); err != nil {
		return fmt.Errorf("migrate v200→v201 (Codex app-server runtime): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v200→v201 (Codex app-server runtime): commit: %w", err)
	}
	return nil
}
