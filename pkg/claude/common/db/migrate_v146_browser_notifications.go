package db

import (
	"database/sql"
	"fmt"
)

// migrateV145toV146 adds browser_notifications — the hand-off queue for
// notifications delivered through the agentd dashboard's Web Notification
// API instead of (or alongside) the platform notifier.
//
// A queue in SQLite rather than an in-process channel because the
// notification decision is made in whichever process observed the
// transition: a short-lived `tclaude` hook callback, the CLI task runner,
// the rate-limit watcher, or agentd itself. SQLite is the one thing all of
// them already share, and it survives an agentd restart between the
// enqueue and the browser's next poll.
//
// Rows are cursor-consumed (each dashboard tab tracks its own last-seen
// id) and pruned by age, so nothing here is authoritative state — it is a
// short-lived outbox.
func migrateV145toV146(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v145→v146 (browser notifications): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS browser_notifications (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL,
			body       TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_browser_notifications_created
			ON browser_notifications(created_at);
	`); err != nil {
		return fmt.Errorf("migrate v145→v146 (create browser_notifications): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 146`); err != nil {
		return fmt.Errorf("migrate v145→v146 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v145→v146 (commit): %w", err)
	}
	return nil
}
