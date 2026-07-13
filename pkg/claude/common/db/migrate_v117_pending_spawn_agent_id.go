package db

import (
	"database/sql"
	"fmt"
)

// migrateV116toV117 gives a pending spawn its tclaude-owned stable actor id
// before the harness conversation id materialises. Legacy rows retain an empty
// id and follow the old allocate-on-enrollment path; every new pending spawn
// reserves a non-empty id and later binds that exact value to its conv. The
// launching bit protects the reservation during the short interval before the
// session wrapper creates its row.
func migrateV116toV117(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v116→v117 (pending spawn agent id): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pending_spawns'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v116→v117 (probe pending_spawns): %w", err)
	}
	if haveTable != 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = 'agent_id'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v116→v117 (probe pending_spawns.agent_id): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE pending_spawns ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v116→v117 (add pending_spawns.agent_id): %w", err)
			}
		}
		var haveLaunching int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = 'launching'`,
		).Scan(&haveLaunching); err != nil {
			return fmt.Errorf("migrate v116→v117 (probe pending_spawns.launching): %w", err)
		}
		if haveLaunching == 0 {
			if _, err := tx.Exec(`ALTER TABLE pending_spawns ADD COLUMN launching INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("migrate v116→v117 (add pending_spawns.launching): %w", err)
			}
		}
		if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_spawns_agent
			ON pending_spawns(agent_id) WHERE agent_id <> ''`); err != nil {
			return fmt.Errorf("migrate v116→v117 (index pending_spawns.agent_id): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 117`); err != nil {
		return fmt.Errorf("migrate v116→v117 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v116→v117 (commit): %w", err)
	}
	return nil
}
