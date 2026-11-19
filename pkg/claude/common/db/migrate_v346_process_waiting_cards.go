package db

import (
	"database/sql"
	"fmt"
)

// migrateV345toV346 adds process_waiting_cards (tk_b9c30e99): the operator's
// Messages card for each thing a process run waits on a person for, a
// decision or a step a person performs, keyed by the run, the step and its
// attempt, so each is put up once and closed when it is settled or withdrawn.
func migrateV345toV346(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v345→v346 (process waiting cards): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS process_waiting_cards (
			run_id           TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE
			                     CHECK(length(CAST(run_id AS BLOB)) BETWEEN 1 AND 128),
			node_id          TEXT NOT NULL CHECK(length(CAST(node_id AS BLOB)) BETWEEN 1 AND 512),
			attempt          INTEGER NOT NULL,
			kind             TEXT NOT NULL CHECK(kind IN ('decision', 'task')),
			human_message_id INTEGER NOT NULL,
			created_at       INTEGER NOT NULL,
			closed_at        INTEGER,
			PRIMARY KEY (run_id, node_id, attempt, kind)
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_process_waiting_cards_message
			ON process_waiting_cards(human_message_id);
		CREATE INDEX IF NOT EXISTS idx_process_waiting_cards_open
			ON process_waiting_cards(run_id) WHERE closed_at IS NULL;
	`); err != nil {
		return fmt.Errorf("migrate v345→v346 (create waiting cards table): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 346`); err != nil {
		return fmt.Errorf("migrate v345→v346 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v345→v346 (commit): %w", err)
	}
	return nil
}
