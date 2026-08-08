package db

import (
	"database/sql"
	"fmt"
)

// migrateV191toV192 records which cron job produced each durable inbox row.
// The scheduler uses this trusted provenance to keep at most one unclaimed,
// undelivered tick from a job buffered for each recipient.
func migrateV191toV192(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v191→v192 (cron message origins): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_cron_messages (
			message_id INTEGER PRIMARY KEY
			           REFERENCES agent_messages(id) ON DELETE CASCADE,
			cron_job_id INTEGER NOT NULL CHECK(cron_job_id > 0)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_cron_messages_job
			ON agent_cron_messages(cron_job_id);
		UPDATE schema_version SET version = 192;
	`); err != nil {
		return fmt.Errorf("migrate v191→v192 (cron message origins): apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v191→v192 (cron message origins): commit: %w", err)
	}
	return nil
}
