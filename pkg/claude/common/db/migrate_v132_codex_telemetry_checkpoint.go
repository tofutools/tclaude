package db

import (
	"database/sql"
	"fmt"
)

// migrateV131toV132 adds the durable cursor/state checkpoint used by the
// dashboard's incremental Codex rollout follower. The row is session-scoped:
// deleting a session removes its checkpoint, while an agentd restart can pick
// up at the last validated complete record instead of rescanning a multi-GB
// long-running rollout from byte zero.
func migrateV131toV132(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v131→v132: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS codex_telemetry_checkpoints (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			data       TEXT NOT NULL,
			failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
			updated_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("migrate v131→v132 (create checkpoints): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 132`); err != nil {
		return fmt.Errorf("migrate v131→v132 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v131→v132 (commit): %w", err)
	}
	return nil
}
