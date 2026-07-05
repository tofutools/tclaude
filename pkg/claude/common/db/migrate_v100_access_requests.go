package db

import (
	"database/sql"
	"fmt"
)

// migrateV99toV100 adds access_requests — the durable recent-history store
// behind the dashboard Messages tab's "Access requests" folder. Pending
// approvals are still actionable only while their waiter lives in agentd memory,
// but every request and terminal outcome is recorded here so handled cards
// survive an agentd restart.
func migrateV99toV100(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v99→v100: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS access_requests (
			id                TEXT PRIMARY KEY,
			perm              TEXT NOT NULL,
			conv_id           TEXT NOT NULL DEFAULT '',
			agent_id          TEXT NOT NULL DEFAULT '',
			conv_title        TEXT NOT NULL DEFAULT '',
			method            TEXT NOT NULL DEFAULT '',
			path              TEXT NOT NULL DEFAULT '',
			raw_query         TEXT NOT NULL DEFAULT '',
			body_preview      TEXT NOT NULL DEFAULT '',
			body_label        TEXT NOT NULL DEFAULT '',
			target_group      TEXT NOT NULL DEFAULT '',
			target_conv_id    TEXT NOT NULL DEFAULT '',
			target_conv_title TEXT NOT NULL DEFAULT '',
			auto_grantable    INTEGER NOT NULL DEFAULT 0,
			status            TEXT NOT NULL DEFAULT 'pending',
			created_at        TEXT NOT NULL,
			deadline_at       TEXT NOT NULL DEFAULT '',
			decided_at        TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_access_requests_status_decided
			ON access_requests(status, decided_at, created_at);
	`); err != nil {
		return fmt.Errorf("migrate v99→v100 (create access_requests): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 100`); err != nil {
		return fmt.Errorf("migrate v99→v100 (version): %w", err)
	}
	return tx.Commit()
}
