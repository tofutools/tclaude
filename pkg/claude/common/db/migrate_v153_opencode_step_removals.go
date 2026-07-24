package db

import (
	"database/sql"
	"fmt"
)

// migrateV152toV153 retains the fact that an OpenCode assistant message once
// had pricing steps after its final step-finish part is removed. OpenCode can
// leave stale top-level tokens in message history after removing every part;
// this conversation-keyed tombstone prevents reconnects and resumed local
// sessions from treating those tokens as live usage again.
func migrateV152toV153(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v152→v153 (OpenCode step removals): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS opencode_usage_step_removals (
			conv_id    TEXT NOT NULL,
			message_id TEXT NOT NULL,
			removed_at TEXT NOT NULL,
			PRIMARY KEY (conv_id, message_id)
		);
		CREATE INDEX IF NOT EXISTS idx_opencode_usage_step_removals_removed
			ON opencode_usage_step_removals(removed_at);
	`); err != nil {
		return fmt.Errorf("migrate v152→v153 (OpenCode step removals): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 153`); err != nil {
		return fmt.Errorf("migrate v152→v153 (OpenCode step removals): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v152→v153 (OpenCode step removals): commit: %w", err)
	}
	return nil
}
