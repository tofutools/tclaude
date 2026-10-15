package db

import "database/sql"

// migrateV260toV261 adds process_agent_dispatches: the daemon's durable record
// of which agent performs each outstanding agent command of a process run.
//
// The run checkpoint stays the authority on WHETHER a command is outstanding;
// this table only records HOW it is being performed — the resolved performer,
// delivery, and the performer's report — so a restarted daemon rediscovers
// every in-flight dispatch without re-resolving or re-delivering it. Rows cascade
// with their run.
func migrateV260toV261(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS process_agent_dispatches (
	run_id         TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE,
	command_id     TEXT NOT NULL CHECK(length(CAST(command_id AS BLOB)) BETWEEN 1 AND 1024),
	node_id        TEXT NOT NULL CHECK(length(CAST(node_id AS BLOB)) BETWEEN 1 AND 512),
	attempt        INTEGER NOT NULL CHECK(attempt >= 1),
	state          TEXT NOT NULL CHECK(state IN ('pending', 'needs_performer', 'delivered', 'reported', 'closed')),
	agent_id       TEXT NOT NULL DEFAULT '',
	resolution     TEXT NOT NULL DEFAULT '',
	wait_reason    TEXT NOT NULL DEFAULT '',
	message_id     INTEGER NOT NULL DEFAULT 0,
	deliveries     INTEGER NOT NULL DEFAULT 0,
	report_outcome TEXT NOT NULL DEFAULT '',
	report_note    TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL,
	delivered_at   INTEGER,
	reported_at    INTEGER,
	PRIMARY KEY (run_id, command_id)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_process_agent_dispatches_agent
	ON process_agent_dispatches(agent_id, state);
CREATE INDEX IF NOT EXISTS idx_process_agent_dispatches_open
	ON process_agent_dispatches(state) WHERE state IN ('pending', 'needs_performer', 'delivered', 'reported');
UPDATE schema_version SET version=261;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
