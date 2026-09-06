package db

import (
	"database/sql"
	"fmt"
)

// migrateV227toV228 creates durable managed Resume operation evidence. It is
// intentionally unregistered until identity migration 227 is integrated on
// the feature branch; registering this step against v226 would violate the
// contiguous migration chain.
func migrateV227toV228(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v227→v228 (resume operations): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS execution_operations (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		agent_id TEXT NOT NULL DEFAULT '',
		conv_id TEXT NOT NULL,
		predecessor_execution_id TEXT NOT NULL DEFAULT '',
		predecessor_session_id TEXT NOT NULL DEFAULT '',
		intended_execution_id TEXT NOT NULL,
		intended_session_id TEXT NOT NULL DEFAULT '',
		logical_conversation_id TEXT NOT NULL DEFAULT '',
		claim_hash TEXT NOT NULL DEFAULT '',
		claim_pid INTEGER NOT NULL DEFAULT 0,
		claim_process_start TEXT NOT NULL DEFAULT '',
		tmux_session TEXT NOT NULL DEFAULT '',
		pane_id TEXT NOT NULL DEFAULT '',
		gate_path TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		launch_phase TEXT NOT NULL,
		revision INTEGER NOT NULL,
		recovery_agent_id TEXT NOT NULL DEFAULT '',
		recovery_generation TEXT NOT NULL DEFAULT '',
		dispatch_detail TEXT NOT NULL DEFAULT '',
		failure_detail TEXT NOT NULL DEFAULT '',
		requested_at INTEGER NOT NULL,
		accepted_at INTEGER,
		started_at INTEGER,
		ready_at INTEGER
	) STRICT`); err != nil {
		return fmt.Errorf("migrate v227→v228 (resume operations table): %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS execution_operations_active
		ON execution_operations(state, requested_at)`); err != nil {
		return fmt.Errorf("migrate v227→v228 (resume operations index): %w", err)
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS execution_operations_one_active_conv
		ON execution_operations(conv_id)
		WHERE state NOT IN ('ready', 'rejected', 'failed', 'cancelled')`); err != nil {
		return fmt.Errorf("migrate v227→v228 (resume active conversation index): %w", err)
	}
	var haveSessions int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&haveSessions); err != nil {
		return fmt.Errorf("migrate v227→v228 (sessions probe): %w", err)
	}
	if haveSessions != 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='resume_operation_id'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v227→v228 (session column probe): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN resume_operation_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v227→v228 (session operation column): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version=228`); err != nil {
		return fmt.Errorf("migrate v227→v228 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v227→v228 (commit): %w", err)
	}
	return nil
}
