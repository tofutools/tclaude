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
			id                     TEXT NOT NULL PRIMARY KEY
			                           CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
			template_ref           TEXT NOT NULL
			                           CHECK(length(CAST(template_ref AS BLOB)) BETWEEN 1 AND 512),
			template_snapshot_json TEXT NOT NULL
			                           CHECK(length(CAST(template_snapshot_json AS BLOB)) BETWEEN 1 AND 4194304),
			params_json            TEXT NOT NULL
			                           CHECK(length(CAST(params_json AS BLOB)) BETWEEN 1 AND 262144),
			status                 TEXT NOT NULL
			                           CHECK(length(CAST(status AS BLOB)) BETWEEN 1 AND 64),
			state_version          INTEGER NOT NULL CHECK(state_version > 0),
			checkpoint_json        TEXT NOT NULL
			                           CHECK(length(CAST(checkpoint_json AS BLOB)) BETWEEN 1 AND 4194304),
			created_at             TEXT NOT NULL
			                           CHECK(length(CAST(created_at AS BLOB)) BETWEEN 1 AND 64),
			updated_at             TEXT NOT NULL
			                           CHECK(length(CAST(updated_at AS BLOB)) BETWEEN 1 AND 64)
		);
		CREATE INDEX IF NOT EXISTS idx_process_runs_active
			ON process_runs(id)
			WHERE status NOT IN ('completed', 'failed', 'canceled');

		CREATE TABLE IF NOT EXISTS process_run_events (
			run_id       TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE
			                 CHECK(length(CAST(run_id AS BLOB)) BETWEEN 1 AND 128),
			sequence     INTEGER NOT NULL CHECK(sequence > 0),
			occurred_at  TEXT NOT NULL
			                 CHECK(length(CAST(occurred_at AS BLOB)) BETWEEN 1 AND 64),
			node_id      TEXT NOT NULL DEFAULT ''
			                 CHECK(length(CAST(node_id AS BLOB)) <= 256),
			kind         TEXT NOT NULL
			                 CHECK(length(CAST(kind AS BLOB)) BETWEEN 1 AND 128),
			payload_json TEXT NOT NULL
			                 CHECK(length(CAST(payload_json AS BLOB)) BETWEEN 1 AND 262144),
			actor        TEXT NOT NULL DEFAULT ''
			                 CHECK(length(CAST(actor AS BLOB)) <= 256),
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
