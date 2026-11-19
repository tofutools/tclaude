package db

import (
	"database/sql"
	"fmt"
)

// migrateV343toV344 adds process_wait_timers (tk_96aee1ac): the deadline of
// each wait step that is waiting, fixed when the step starts so that a daemon
// restart keeps it. A row with a failure says the wait's deadline could not be
// worked out; the runtime fails the step with it.
func migrateV343toV344(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v343→v344 (wait timers): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS process_wait_timers (
	run_id     TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE,
	command_id TEXT NOT NULL CHECK(length(CAST(command_id AS BLOB)) BETWEEN 1 AND 1024),
	node_id    TEXT NOT NULL CHECK(length(CAST(node_id AS BLOB)) BETWEEN 1 AND 512),
	attempt    INTEGER NOT NULL CHECK(attempt >= 1),
	started_at INTEGER NOT NULL,
	due_at     INTEGER NOT NULL,
	failure    TEXT NOT NULL DEFAULT '' CHECK(length(CAST(failure AS BLOB)) <= 1024),
	PRIMARY KEY (run_id, command_id)
) STRICT`); err != nil {
		return fmt.Errorf("migrate v343→v344 (create table): %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_process_wait_timers_due ON process_wait_timers(due_at)`); err != nil {
		return fmt.Errorf("migrate v343→v344 (index): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 344`); err != nil {
		return fmt.Errorf("migrate v343→v344 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v343→v344 (commit): %w", err)
	}
	return nil
}
