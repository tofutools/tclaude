package db

import (
	"database/sql"
	"fmt"
)

// migrateV98toV99 adds agent_groups.parent_id — a nullable self-reference
// that nests one group under another (n-level groups-in-groups, JOH-392).
// NULL = top-level group (the pre-column default for every existing row).
//
// Reference by ID, not name: it survives a group rename for free (the
// same reason agent_group_links keys on FromGroupID/ToGroupID), and — the
// crux — it lets SQLite own the "parent disappeared" case. The column is
//
//	parent_id INTEGER REFERENCES agent_groups(id) ON DELETE SET NULL
//
// so deleting a parent group auto-nulls its children's parent_id (they pop
// back to top-level) instead of leaving a dangling pointer. Foreign keys
// are enforced on every connection (see db.go's DSN _pragma=foreign_keys(1)),
// and DeleteAgentGroup already runs its DELETE inside a normal tx on such a
// connection, so the SET NULL fires without any extra application code.
//
// v1 nesting is STRUCTURE ONLY — it groups the dashboard board and cross-
// syncs as server truth, but does NOT (yet) inherit permissions, message
// routing, cron multicast, or spawn-target down the tree. Those are
// deliberate follow-ups; the column just records the shape.
//
// Additive + idempotent (the v76+ convention): the table is probed so a
// partial-schema heal DB missing it is a clean skip; the ADD COLUMN is
// guarded by a pragma_table_info probe so a half-applied run converges on
// re-run instead of wedging on "duplicate column". A nullable FK column
// added via ALTER TABLE ADD COLUMN is legal in SQLite precisely because its
// default is NULL (a NOT NULL / non-NULL-default FK column cannot be added
// this way). Rides one transaction with the version bump; no VACUUM — nothing
// is dropped or rewritten.
func migrateV98toV99(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v98→v99: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agent_groups")
	if err != nil {
		return fmt.Errorf("migrate v98→v99 (probe agent_groups): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'parent_id'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v98→v99 (probe column): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agent_groups ADD COLUMN parent_id INTEGER REFERENCES agent_groups(id) ON DELETE SET NULL`,
			); err != nil {
				return fmt.Errorf("migrate v98→v99 (add column): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 99`); err != nil {
		return fmt.Errorf("migrate v98→v99 (version): %w", err)
	}
	return tx.Commit()
}
