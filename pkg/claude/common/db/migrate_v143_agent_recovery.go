package db

import (
	"database/sql"
	"fmt"
)

// migrateV142toV143 adds the durable, stable-agent keyed recovery ledger used
// to resume managed Codex conversations after an eligible nonzero runtime exit.
// One row is enough per actor: predecessor_generation is the CAS authority for
// the current recovery episode and a later launch replaces it atomically.
func migrateV142toV143(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v142→v143 (agent recovery): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_recovery (
			agent_id TEXT PRIMARY KEY REFERENCES agents(agent_id) ON DELETE CASCADE,
			conv_id TEXT NOT NULL,
			predecessor_session_id TEXT NOT NULL,
			predecessor_generation TEXT NOT NULL,
			exit_event_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			reason_code TEXT NOT NULL DEFAULT '',
			consecutive_crashes INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_crashes >= 0),
			backoff_step INTEGER NOT NULL DEFAULT 0 CHECK(backoff_step >= 0),
			next_attempt_at TEXT NOT NULL DEFAULT '',
			backoff_seconds INTEGER NOT NULL DEFAULT 0 CHECK(backoff_seconds >= 0),
			lease_token TEXT NOT NULL DEFAULT '',
			lease_expires_at TEXT NOT NULL DEFAULT '',
			attempt_started_at TEXT NOT NULL DEFAULT '',
			successor_session_id TEXT NOT NULL DEFAULT '',
			successor_generation TEXT NOT NULL DEFAULT '',
			last_exit_code INTEGER,
			last_exit_signal TEXT NOT NULL DEFAULT '',
			last_exit_at TEXT NOT NULL DEFAULT '',
			recovered_at TEXT NOT NULL DEFAULT '',
			healthy_since TEXT NOT NULL DEFAULT '',
			notified_crash INTEGER NOT NULL DEFAULT 0,
			notified_backoff INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_recovery_due
			ON agent_recovery(status, next_attempt_at);
	`); err != nil {
		return fmt.Errorf("migrate v142→v143 (agent recovery): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 143`); err != nil {
		return fmt.Errorf("migrate v142→v143 (agent recovery): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v142→v143 (agent recovery): commit: %w", err)
	}
	return nil
}
