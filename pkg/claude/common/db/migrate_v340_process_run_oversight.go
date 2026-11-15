package db

import (
	"database/sql"
	"fmt"
)

// migrateV339toV340 adds run oversight (tk_e2fa2bd0): the overseer a run was
// given explicitly, and the concerns raised about a run on their way up the
// escalation chain (overseer, then the group's escalation agent, then the
// operator).
//
// The overseer lives in a side table rather than on process_runs: a run
// without a row takes the default overseer (the group's lead, else whoever
// started it), which is resolved when it is needed, so existing runs need no
// backfill.
func migrateV339toV340(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v339→v340 (process run oversight): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS process_run_overseers (
			run_id   TEXT PRIMARY KEY REFERENCES process_runs(id) ON DELETE CASCADE
			             CHECK(length(CAST(run_id AS BLOB)) BETWEEN 1 AND 128),
			overseer TEXT NOT NULL
			             CHECK(length(CAST(overseer AS BLOB)) BETWEEN 1 AND 256),
			set_by   TEXT NOT NULL DEFAULT ''
			             CHECK(length(CAST(set_by AS BLOB)) <= 256),
			set_at   INTEGER NOT NULL
		) STRICT;

		CREATE TABLE IF NOT EXISTS process_run_concerns (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id           TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE
			                     CHECK(length(CAST(run_id AS BLOB)) BETWEEN 1 AND 128),
			node_id          TEXT NOT NULL DEFAULT ''
			                     CHECK(length(CAST(node_id AS BLOB)) <= 256),
			source           TEXT NOT NULL CHECK(source IN ('agent', 'operator', 'engine')),
			signal           TEXT NOT NULL DEFAULT ''
			                     CHECK(length(CAST(signal AS BLOB)) <= 64),
			raised_by        TEXT NOT NULL
			                     CHECK(length(CAST(raised_by AS BLOB)) BETWEEN 1 AND 256),
			reason           TEXT NOT NULL
			                     CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 4096),
			state            TEXT NOT NULL CHECK(state IN ('open', 'resolved')),
			level            TEXT NOT NULL CHECK(level IN ('overseer', 'escalation', 'operator')),
			holder           TEXT NOT NULL DEFAULT ''
			                     CHECK(length(CAST(holder AS BLOB)) <= 256),
			answer_by        INTEGER,
			human_message_id INTEGER NOT NULL DEFAULT 0,
			resolution       TEXT NOT NULL DEFAULT ''
			                     CHECK(length(CAST(resolution AS BLOB)) <= 64),
			resolved_by      TEXT NOT NULL DEFAULT ''
			                     CHECK(length(CAST(resolved_by AS BLOB)) <= 256),
			note             TEXT NOT NULL DEFAULT ''
			                     CHECK(length(CAST(note AS BLOB)) <= 4096),
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL,
			resolved_at      INTEGER
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_process_run_concerns_run
			ON process_run_concerns(run_id, id);
		CREATE INDEX IF NOT EXISTS idx_process_run_concerns_due
			ON process_run_concerns(answer_by)
			WHERE state = 'open' AND answer_by IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_process_run_concerns_message
			ON process_run_concerns(human_message_id)
			WHERE human_message_id != 0;
	`); err != nil {
		return fmt.Errorf("migrate v339→v340 (create oversight tables): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 340`); err != nil {
		return fmt.Errorf("migrate v339→v340 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v339→v340 (commit): %w", err)
	}
	return nil
}
