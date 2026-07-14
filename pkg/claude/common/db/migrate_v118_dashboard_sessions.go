package db

import (
	"database/sql"
	"fmt"
)

// migrateV117toV118 adds the short-lived handoff store used to carry a
// dashboard browser session across a clean agentd restart. Only a SHA-256
// digest of the cookie is stored; possession of the database does not reveal a
// cookie value that can be replayed in a browser.
func migrateV117toV118(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v117→v118 (dashboard session grace): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS dashboard_session_grace (
			token_hash TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_dashboard_session_grace_expiry
			ON dashboard_session_grace(expires_at);
	`); err != nil {
		return fmt.Errorf("migrate v117→v118 (create dashboard_session_grace): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 118`); err != nil {
		return fmt.Errorf("migrate v117→v118 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v117→v118 (commit): %w", err)
	}
	return nil
}
