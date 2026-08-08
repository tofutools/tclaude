package db

import (
	"database/sql"
	"fmt"
)

// migrateV196toV197 adds the append-only parent→child facts used by scoped
// permission selectors. The table deliberately has no foreign keys: lineage is
// historical authorization evidence and must survive retirement or deletion of
// either endpoint instead of cascading away with the mutable agent roster.
//
// There is intentionally no historical backfill. agents.initial_spawn_config
// stores the verbatim SpawnRequest, which has a reply_to field but no spawner
// identity. reply_to may name a third party and is omitted on the common default
// path, so deriving lineage from it could fabricate retire authority. Missing
// edges fail closed; guessed edges would fail open.
func migrateV196toV197(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v196→v197 (agent lineage): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_lineage (
			child_agent_id  TEXT PRIMARY KEY,
			parent_agent_id TEXT NOT NULL,
			spawned_at      INTEGER NOT NULL
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_agent_lineage_parent
			ON agent_lineage(parent_agent_id, child_agent_id);
	`); err != nil {
		return fmt.Errorf("migrate v196→v197 (agent lineage): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 197`); err != nil {
		return fmt.Errorf("migrate v196→v197 (agent lineage): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v196→v197 (agent lineage): commit: %w", err)
	}
	return nil
}
