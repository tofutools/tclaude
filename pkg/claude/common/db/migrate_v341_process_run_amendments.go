package db

import (
	"database/sql"
	"fmt"
)

// migrateV340toV341 adds run amendments (tk_e2fa2bd0 3c): changes to one
// run's own copy of its template, proposed and then applied or rejected.
// The run's template_snapshot_json is swapped when an amendment is applied;
// each row keeps the copy it proposed and the run's state version it was
// proposed against, so an amendment formed against a run that has since
// moved on is refused rather than applied to a frontier it never saw.
func migrateV340toV341(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v340→v341 (process run amendments): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS process_run_amendments (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id             TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE
			                       CHECK(length(CAST(run_id AS BLOB)) BETWEEN 1 AND 128),
			state              TEXT NOT NULL CHECK(state IN ('proposed', 'applied', 'rejected', 'stale')),
			proposed_by        TEXT NOT NULL
			                       CHECK(length(CAST(proposed_by AS BLOB)) BETWEEN 1 AND 256),
			note               TEXT NOT NULL DEFAULT ''
			                       CHECK(length(CAST(note AS BLOB)) <= 4096),
			base_state_version INTEGER NOT NULL,
			template_json      TEXT NOT NULL,
			summary_json       TEXT NOT NULL DEFAULT '{}',
			human_message_id   INTEGER NOT NULL DEFAULT 0,
			decided_by         TEXT NOT NULL DEFAULT ''
			                       CHECK(length(CAST(decided_by AS BLOB)) <= 256),
			decision_note      TEXT NOT NULL DEFAULT ''
			                       CHECK(length(CAST(decision_note AS BLOB)) <= 4096),
			saved_template_ref TEXT NOT NULL DEFAULT ''
			                       CHECK(length(CAST(saved_template_ref AS BLOB)) <= 512),
			created_at         INTEGER NOT NULL,
			decided_at         INTEGER
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_process_run_amendments_run
			ON process_run_amendments(run_id, id);
		CREATE INDEX IF NOT EXISTS idx_process_run_amendments_message
			ON process_run_amendments(human_message_id)
			WHERE human_message_id != 0;
	`); err != nil {
		return fmt.Errorf("migrate v340→v341 (create amendments table): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 341`); err != nil {
		return fmt.Errorf("migrate v340→v341 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v340→v341 (commit): %w", err)
	}
	return nil
}
