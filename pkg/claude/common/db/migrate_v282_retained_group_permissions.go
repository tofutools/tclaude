package db

import (
	"database/sql"
	"fmt"
)

// migrateV281toV282 adds agent_retained_group_permissions: the groups whose
// group permissions an agent keeps after an operator moved it out of them
// with "keep permissions". A row grants exactly what membership of that group
// would (its live grants, bound to that group), never a member's other
// standing. Deleting the group drops the row.
func migrateV281toV282(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v281→v282 (retained group permissions): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS agent_retained_group_permissions (
			agent_id    TEXT NOT NULL,
			group_id    INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			retained_at INTEGER NOT NULL,
			retained_by TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (agent_id, group_id)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_agent_retained_group_permissions_group
			ON agent_retained_group_permissions(group_id)`,
		`UPDATE schema_version SET version = 282`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v281→v282 (retained group permissions): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v281→v282 (commit): %w", err)
	}
	return nil
}
