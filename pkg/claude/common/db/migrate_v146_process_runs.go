package db

import (
	"database/sql"
	"fmt"
)

// migrateV145toV146 creates the replacement Processes runtime store. The
// checkpoint row is canonical; process_run_events is human-facing evidence and
// is never replayed to reconstruct state. Runtime rows deliberately have no
// foreign key to the filesystem-backed template authoring store: a run pins an
// exact semantic snapshot and therefore remains readable if its template file
// is edited or deleted.
func migrateV145toV146(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v145→v146 (process runs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS process_runs (
			id                     TEXT PRIMARY KEY,
			template_ref           TEXT NOT NULL,
			template_snapshot_json TEXT NOT NULL,
			params_json            TEXT NOT NULL,
			status                 TEXT NOT NULL,
			state_version          INTEGER NOT NULL CHECK(state_version > 0),
			checkpoint_json        TEXT NOT NULL,
			created_at             TEXT NOT NULL,
			updated_at             TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_process_runs_active
			ON process_runs(id)
			WHERE status NOT IN ('completed', 'failed', 'canceled');

		CREATE TABLE IF NOT EXISTS process_run_events (
			run_id       TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE,
			sequence     INTEGER NOT NULL CHECK(sequence > 0),
			occurred_at  TEXT NOT NULL,
			node_id      TEXT NOT NULL DEFAULT '',
			kind         TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			actor        TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (run_id, sequence)
		);
	`); err != nil {
		return fmt.Errorf("migrate v145→v146 (create process runtime tables): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 146`); err != nil {
		return fmt.Errorf("migrate v145→v146 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v145→v146 (commit): %w", err)
	}
	return nil
}
