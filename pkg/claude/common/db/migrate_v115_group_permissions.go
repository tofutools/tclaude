package db

import (
	"database/sql"
	"fmt"
)

// migrateV114toV115 adds live, additive permission grants scoped to group
// membership. Unlike spawn-profile permission overrides these rows are not
// copied onto an actor: joining/leaving a group changes the effective set
// immediately, and deleting the group removes its grants by cascade.
func migrateV114toV115(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v114→v115 (group permissions): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_group_permissions (
			group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			slug       TEXT NOT NULL,
			granted_at TEXT NOT NULL,
			granted_by TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (group_id, slug)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_group_permissions_slug
			ON agent_group_permissions(slug);
		UPDATE schema_version SET version = 115;
	`); err != nil {
		return fmt.Errorf("migrate v114→v115 (group permissions): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v114→v115 (group permissions): commit: %w", err)
	}
	return nil
}
