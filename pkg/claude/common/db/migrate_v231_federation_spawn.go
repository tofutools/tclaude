package db

import (
	"database/sql"
	"fmt"
)

// migrateV230toV231 adds federation_spawn_requests: spawn requests a
// trusted peer sent into one of this instance's exported groups. A request
// only ever waits for the local operator's decision; nothing spawns until
// they approve it.
func migrateV230toV231(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v230→v231 (federation spawn): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(federationSpawnSchema); err != nil {
		return fmt.Errorf("migrate v230→v231 (federation spawn): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 231`); err != nil {
		return fmt.Errorf("migrate v230→v231 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v230→v231 (commit): %w", err)
	}
	return nil
}

const federationSpawnSchema = `
CREATE TABLE IF NOT EXISTS federation_spawn_requests (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	from_instance TEXT NOT NULL,
	envelope_id   TEXT NOT NULL,
	from_agent    TEXT NOT NULL DEFAULT '',
	from_name     TEXT NOT NULL DEFAULT '',
	group_id      INTEGER NOT NULL,
	group_name    TEXT NOT NULL,
	name          TEXT NOT NULL DEFAULT '',
	role          TEXT NOT NULL DEFAULT '',
	brief         TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	result_agent  TEXT NOT NULL DEFAULT '',
	reason        TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	expires_at    INTEGER NOT NULL,
	decided_at    INTEGER,
	UNIQUE (from_instance, envelope_id)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_federation_spawn_requests_status
	ON federation_spawn_requests(status, from_instance);
`
