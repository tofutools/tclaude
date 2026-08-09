package db

import (
	"database/sql"
	"fmt"
)

// migrateV203toV204 adds daemon-private, restart-durable Codex app-server
// capabilities. Keeping credentials in their own table prevents ordinary
// runtime/diagnostic reads from ever carrying them.
func migrateV203toV204(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v203→v204 (Codex app-server capabilities): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS codex_app_server_capabilities (
			generation TEXT PRIMARY KEY,
			capability TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (generation) REFERENCES codex_app_server_runtimes(generation) ON DELETE CASCADE
		) STRICT;
		CREATE TRIGGER IF NOT EXISTS codex_app_server_capability_terminal_cleanup
		AFTER UPDATE OF state ON codex_app_server_runtimes
		WHEN NEW.state IN ('unavailable', 'dead')
		BEGIN
			DELETE FROM codex_app_server_capabilities WHERE generation = NEW.generation;
		END;
	`); err != nil {
		return fmt.Errorf("migrate v203→v204 (Codex app-server capabilities): create table: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 204`); err != nil {
		return fmt.Errorf("migrate v203→v204 (Codex app-server capabilities): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v203→v204 (Codex app-server capabilities): commit: %w", err)
	}
	return nil
}
