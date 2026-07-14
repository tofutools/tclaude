package db

import (
	"database/sql"
	"fmt"
)

// migrateV118toV119 adds the durable response ledger for mutating agentd
// requests. A client keeps one request key across transport retries; completed
// responses survive an agentd restart and can be replayed without running the
// mutation twice. Pending rows make a crash-before-recording explicit instead
// of silently guessing that a retry is safe.
func migrateV118toV119(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v118→v119 (agentd idempotency): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agentd_idempotency (
			request_key TEXT PRIMARY KEY,
			fingerprint TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending', 'completed')),
			status INTEGER NOT NULL DEFAULT 0,
			headers_json TEXT NOT NULL DEFAULT '',
			response_body BLOB,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agentd_idempotency_expiry
			ON agentd_idempotency(expires_at);
	`); err != nil {
		return fmt.Errorf("migrate v118→v119 (create agentd_idempotency): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 119`); err != nil {
		return fmt.Errorf("migrate v118→v119 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v118→v119 (commit): %w", err)
	}
	return nil
}
