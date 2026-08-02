package db

import (
	"database/sql"
	"fmt"
)

// migrateV184toV185 adds the launch-owned Darwin route contract.  It is kept
// separate from agent_routes because a route may be created by one launch and
// consumed by another; each launch must prove its own exact slot allocation.
func migrateV184toV185(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v184→v185 (Darwin route launches): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS darwin_route_launches (
			agent_id TEXT NOT NULL,
			conv_id TEXT NOT NULL,
			launch_generation TEXT NOT NULL,
			slots TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending', 'active', 'closed')),
			created_at INTEGER NOT NULL,
			closed_at INTEGER,
			PRIMARY KEY(agent_id, conv_id, launch_generation)
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_darwin_route_launches_identity
			ON darwin_route_launches(agent_id, conv_id, launch_generation, state);
		UPDATE schema_version SET version = 185;
	`); err != nil {
		return fmt.Errorf("migrate v184→v185 (Darwin route launches): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v184→v185 (Darwin route launches): commit: %w", err)
	}
	return nil
}
