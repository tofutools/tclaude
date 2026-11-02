package db

import "database/sql"

// migrateV299toV300 adds the capacity spawn queue (tcl-ao1yz5). A spawn that
// would go past the configured capacity waits here, first in first out, when
// capacity.when_full is "queue". request_body holds the spawn request as sent,
// which may carry environment values, so it is cleared once the entry ends and
// is never part of a listing.
func migrateV299toV300(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS spawn_queue (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		group_id         INTEGER NOT NULL,
		name             TEXT NOT NULL DEFAULT '',
		requester_kind   TEXT NOT NULL,
		requester_agent  TEXT NOT NULL DEFAULT '',
		requester_name   TEXT NOT NULL DEFAULT '',
		requester_member INTEGER NOT NULL DEFAULT 0,
		request_body     TEXT,
		proof_dirs       TEXT NOT NULL DEFAULT '',
		state            TEXT NOT NULL,
		reason           TEXT NOT NULL DEFAULT '',
		reserved_agent   TEXT NOT NULL DEFAULT '',
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	) STRICT`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_spawn_queue_state ON spawn_queue (state, id)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 300`); err != nil {
		return err
	}
	return tx.Commit()
}
