package db

import (
	"database/sql"
	"fmt"
)

func migrateV226toV227(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v226→v227: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS awb_ready_dispatches (
		workspace TEXT PRIMARY KEY,
		issue_id TEXT NOT NULL,
		phase TEXT NOT NULL CHECK (phase IN ('selected','claimed','spawned')),
		agent_id TEXT NOT NULL,
		latest_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("migrate v226→v227: create awb_ready_dispatches: %w", err)
	}
	if _, err = tx.Exec(`UPDATE schema_version SET version = 227`); err != nil {
		return err
	}
	return tx.Commit()
}
