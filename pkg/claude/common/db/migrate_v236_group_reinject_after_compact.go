package db

import (
	"database/sql"
	"fmt"
)

// migrateV235toV236 adds agent_groups.reinject_after_compact: what tclaude
// re-injects into a member agent after a compaction or /clear boundary.
// '' is the default (ReinjectContexts); see the ReinjectAfterCompact* values.
func migrateV235toV236(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v235→v236 (group reinject_after_compact): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Probe first, like v206: minimal fixtures (and a re-run after an
	// interrupted attempt) must converge rather than fail.
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_groups'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v235→v236 (group reinject_after_compact): table probe: %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'reinject_after_compact'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v235→v236 (group reinject_after_compact): column probe: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE agent_groups ADD COLUMN reinject_after_compact TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v235→v236 (group reinject_after_compact): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 236`); err != nil {
		return fmt.Errorf("migrate v235→v236 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v235→v236 (commit): %w", err)
	}
	return nil
}
