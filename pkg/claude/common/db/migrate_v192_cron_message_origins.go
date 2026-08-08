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

		-- Best-effort provenance recovery for ticks buffered before this
		-- migration. Cron's server-authored envelope plus its exact stored
		-- routing/payload fields distinguishes scheduler rows from bounded
		-- regular sends. If duplicate jobs are identical, attribute the row to
		-- the newest one; either way the stale flood is safely coalesced.
		INSERT OR IGNORE INTO agent_cron_messages (message_id, cron_job_id)
		SELECT am.id, (
			SELECT j.id
			FROM agent_cron_jobs j
			WHERE am.regular_send = 0
			  AND am.from_agent = j.owner_agent
			  AND am.group_id = j.group_id
			  AND am.body = j.body
			  AND am.subject =
				(CASE WHEN j.name != '' THEN '[cron:' || j.name || '] ' ELSE '[cron] ' END) ||
				(CASE WHEN j.subject != '' THEN j.subject ELSE 'cron' END)
			  AND (
				(j.target_kind = 'conv' AND am.to_agent = j.target_agent AND am.pin_gen = 0)
				OR (j.target_kind = 'group' AND j.group_id != 0)
			  )
			ORDER BY j.id DESC
			LIMIT 1
		)
		FROM agent_messages am
		WHERE am.delivered_at IS NULL AND am.read_at IS NULL
		  AND am.nudge_claimed_at IS NULL AND am.nudge_cancelled_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM agent_cron_jobs j
			WHERE am.regular_send = 0
			  AND am.from_agent = j.owner_agent
			  AND am.group_id = j.group_id
			  AND am.body = j.body
			  AND am.subject =
				(CASE WHEN j.name != '' THEN '[cron:' || j.name || '] ' ELSE '[cron] ' END) ||
				(CASE WHEN j.subject != '' THEN j.subject ELSE 'cron' END)
			  AND (
				(j.target_kind = 'conv' AND am.to_agent = j.target_agent AND am.pin_gen = 0)
				OR (j.target_kind = 'group' AND j.group_id != 0)
			  )
		  );

		-- Keep the newest buffered row for each recovered job/recipient. Rows
		-- already delivered, claimed, or cancelled were not recovered above and
		-- therefore remain untouched.
		DELETE FROM agent_messages
		WHERE id IN (
			SELECT older.message_id
			FROM agent_cron_messages older
			JOIN agent_messages older_message ON older_message.id = older.message_id
			WHERE EXISTS (
				SELECT 1
				FROM agent_cron_messages newer
				JOIN agent_messages newer_message ON newer_message.id = newer.message_id
				WHERE newer.cron_job_id = older.cron_job_id
				  AND newer.message_id > older.message_id
				  AND (
					(older_message.to_agent != '' AND older_message.pin_gen = 0
					 AND newer_message.to_agent = older_message.to_agent AND newer_message.pin_gen = 0)
					OR
					(older_message.to_agent = '' AND newer_message.to_agent = ''
					 AND newer_message.to_conv = older_message.to_conv)
				  )
			)
		);
		UPDATE schema_version SET version = 192;
	`); err != nil {
		return fmt.Errorf("migrate v191→v192 (cron message origins): apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v191→v192 (cron message origins): commit: %w", err)
	}
	return nil
}
